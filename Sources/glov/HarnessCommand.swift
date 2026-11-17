import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct HarnessCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "harness", abstract: "Install the coding-agent harnesses of an isolated VM (vm create --harness), or add one.",
        subcommands: [Install.self, Add.self])

    struct Install: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Install the VM's harnesses under their install allowlist, then take that allowlist away again.",
            discussion: """
                The VM must be running with its Guest Service ready; the install waits up to 2 minutes for the console sign-in, which \
                follows a few seconds later. The hosts the harnesses install from are allowed for the \
                install only; each install script runs as the guest user and puts its command in /usr/local/bin. Whatever \
                happens, the allowlist ends as it was, also when glov is interrupted (Ctrl-C, SIGTERM, a closed terminal or pipe); \
                only killing it outright (SIGKILL) leaves the install hosts, which glov egress list shows. Then take the template snapshot (glov snapshot create <vm> ready) and \
                start each task from it (glov snapshot revert <vm> ready), which also restores the allowlist of the snapshot.

                Then it copies your own harness config from this Mac into the guest user's home (instructions, rules, skills, \
                agents and commands; never settings, hooks or credentials), following symbolic links, and replacing an earlier \
                copy. It is a one-way copy: nothing is written back. --no-host-config copies none of it.
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Flag(name: .customLong("no-host-config"), help: "Do not copy your harness config from this Mac into the VM.") var noHostConfig = false
        @Option(name: .customLong("harness"), help: "Install only this one of the VM's harnesses. Repeatable.") var only: [String] = []
        static let signInTimeout: Duration = .seconds(120)

        func run() async throws {
            try await report {
                let home = GlovHome.resolve()
                let name = try home.resolve(vm)
                let config = try home.loadConfig(name)
                guard config.isolated else {
                    throw Failure("VM \(name) is not isolated: a harness VM is created with glov vm create \(name) --engine vz --isolated --harness <name>")
                }
                guard let names = config.harnesses, !names.isEmpty else { throw Failure("VM \(name) has no harness: it was created without --harness") }
                if let other = only.first(where: { !names.contains($0) }) {
                    throw Failure("VM \(name) has no harness \(other) (it has \(names.joined(separator: ", "))); glov vm harness add \(name) \(other) adds it")
                }
                let profiles = try names.filter { only.isEmpty || only.contains($0) }.map { try HarnessProfile.named($0) }

                let client = daemonClient()
                // A harness VM signs its user in on the console a few seconds after the Guest Service is ready, and the
                // install runs as that user; wait for it before allowing the install hosts.
                do {
                    _ = try await retryingWhileNoUser(timeout: Self.signInTimeout, waiting: { say("\(name): waiting for the console sign-in") }) {
                        try await runAsUser(client, name, ["true"])
                    }
                } catch let e as RPCError where e.code == RPCError.noUser {
                    throw Failure("nobody signed in to the console of \(name) within \(Self.signInTimeout); a harness VM signs in by itself, so look at its console with glov view \(name)")
                }
                let before = try await client.call(Method.egressList, Params.Egress(vm: name), as: Results.Egress.self).allow
                var hosts: [String] = []
                for host in profiles.flatMap(\.install) where !before.contains(host) && !hosts.contains(host) { hosts.append(host) }
                let added = hosts
                // Ctrl-C, a closed terminal or a reader that goes away must not leave the install hosts allowed.
                let interrupted = OnInterrupt {
                    _ = try? await client.call(Method.egressRemove, Params.Egress(vm: name, hosts: added), as: Results.Egress.self)
                    FileHandle.standardError.write(Data("glov: interrupted; install hosts removed from \(name)\n".utf8))
                }
                defer { interrupted.cancel() }
                if !added.isEmpty {
                    _ = try await client.call(Method.egressAllow, Params.Egress(vm: name, hosts: added), as: Results.Egress.self)
                    say("\(name): allowed for the install: \(added.joined(separator: ", "))")
                }
                var failure: (any Error)?
                do {
                    for p in profiles {
                        say("installing \(p.title) (its download can take a few minutes)")
                        // Some installers print nothing while they download; say the install still runs.
                        let started = ContinuousClock.now
                        let ticker = Task {
                            while true {
                                try await Task.sleep(for: .seconds(30))
                                let seconds = Int((ContinuousClock.now - started).components.seconds)
                                say("still installing \(p.title) (\(seconds / 60)m \(String(format: "%02d", seconds % 60))s)")
                            }
                        }
                        let code: Int
                        do { code = try await runAsUser(client, name, ["/bin/sh", "-c", p.installScript]); ticker.cancel() } catch { ticker.cancel(); throw error }
                        guard code == 0 else { throw Failure("installing \(p.name) failed with exit code \(code)") }
                        guard try await runAsUser(client, name, [p.command, "--version"]) == 0 else { throw Failure("\(p.command) --version failed after the install") }
                    }
                } catch { failure = error }
                if !added.isEmpty {
                    do {
                        let r = try await client.call(Method.egressRemove, Params.Egress(vm: name, hosts: added), as: Results.Egress.self)
                        say("\(name): install hosts removed; \(describeAllowlist(r))")
                    } catch {
                        let what = failure.map { "; the install had failed too: \($0)" } ?? ""
                        throw Failure("could not remove the install hosts (\(error)); run glov egress remove \(name) \(added.joined(separator: " "))\(what)")
                    }
                }
                if let failure { throw failure }
                if noHostConfig {
                    say("\(name): host config not copied (--no-host-config)")
                } else {
                    try await copyHostConfig(client, home: home, vm: name, profiles: profiles)
                }
                if let records = config.shares.first(where: { $0.name == HarnessSessions.shareName }) {
                    for p in profiles {
                        for dir in p.sessions ?? [] {
                            guard try await runAsUser(client, name, ["/bin/sh", "-c", HarnessSessions.linkScript(harness: p.name, dir: dir)]) == 0 else {
                                throw Failure("could not link ~/\(dir) to the conversation records share; glov vm status \(name) shows whether \(HarnessSessions.guestPath) is mounted")
                            }
                        }
                    }
                    say("\(name): conversation records go to \(records.hostPath)")
                } else {
                    for dir in profiles.flatMap({ $0.sessions ?? [] }) {
                        _ = try await runAsUser(client, name, ["/bin/sh", "-c", HarnessSessions.unlinkScript(dir: dir)])
                    }
                }
                // A project's VM (`glov <harness>`) is never reverted, so it takes no template snapshot.
                if !config.isProjectVM {
                    say("next: glov snapshot create \(name) ready; start each task with glov snapshot revert \(name) ready")
                }
            }
        }

        /// Copies the profiles' `hostConfig` from this Mac's home into the guest's, through a staging copy in the glov
        /// home's scratch folder (the only host tree a copy into a VM may read), with links already followed.
        private func copyHostConfig(_ client: DaemonClient, home: GlovHome, vm: String, profiles: [HarnessProfile]) async throws {
            var copies: [HarnessProfile.HostCopy] = []
            for c in profiles.flatMap({ $0.hostConfig ?? [] }) where !copies.contains(where: { $0.guest == c.guest }) { copies.append(c) }
            guard !copies.isEmpty else { return }
            let scratch = home.path + "/scratch"
            try FileManager.default.createDirectory(atPath: scratch, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let staging = scratch + "/host-config-" + UUID().uuidString
            try FileManager.default.createDirectory(atPath: staging, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            defer { try? FileManager.default.removeItem(atPath: staging) }
            let report = try HostConfig.stage(copies, hostHome: NSHomeDirectory(), into: staging)
            let guestHome = "/home/\(CloudInitSeed.user)"
            // Replace an earlier copy whole, so what was removed on the Mac goes from the guest too.
            let targets = copies.map { guestHome + "/" + $0.guest }
            guard try await runAs(client, vm, ["rm", "-rf", "--"] + targets, as: "user") == 0 else { throw Failure("could not remove the earlier host config copy in \(vm)") }
            for c in report.copied {
                _ = try await client.call(Method.copyToVM, Params.CopyToVM(vm: vm, hostPath: staging + "/" + c.guest, guestPath: guestHome + "/" + c.guest), as: Results.Copy.self)
            }
            // The Guest Service writes as root; the copy and any folder it made are the guest user's.
            let tops = Array(Swift.Set(report.copied.map { guestHome + "/" + String($0.guest.split(separator: "/")[0]) })).sorted()
            if !tops.isEmpty, try await runAs(client, vm, ["chown", "-R", "\(CloudInitSeed.user):\(CloudInitSeed.user)", "--"] + tops, as: "system") != 0 {
                throw Failure("could not give the host config copy to \(CloudInitSeed.user) in \(vm)")
            }
            for c in report.copied { say("\(vm): copied ~/\(c.guest) (\(c.files) files, \(Self.size(c.bytes)))") }
            for s in report.skipped { say("\(vm): left out \(s)") }
            if report.copied.isEmpty { say("\(vm): no host config to copy") }
        }

        static func size(_ bytes: Int) -> String {
            bytes < 1 << 20 ? "\((bytes + 1023) >> 10) KiB" : String(format: "%.1f MiB", Double(bytes) / Double(1 << 20))
        }

        /// Unbuffered, like the job output followJob writes, so the lines stay in order with it.
        private func say(_ line: String) { try? FileHandle.standardOutput.write(contentsOf: Data((line + "\n").utf8)) }

        private func runAsUser(_ client: DaemonClient, _ vm: String, _ argv: [String]) async throws -> Int {
            try await runAs(client, vm, argv, as: "user")
        }

        private func runAs(_ client: DaemonClient, _ vm: String, _ argv: [String], as runAs: String) async throws -> Int {
            let job = try await client.call(Method.execStart, Params.Exec(vm: vm, argv: argv, cwd: nil, as: runAs), as: Results.JobStarted.self).jobId
            return try await followJob(client, vm: vm, job: job)
        }
    }

    struct Failure: Error, CustomStringConvertible {
        var description: String
        init(_ description: String) { self.description = description }
    }
}

/// Until `cancel`, SIGINT, SIGTERM and SIGHUP run `undo` (for at most 10 s) and exit with 128 + the signal. SIGPIPE is
/// ignored, so a reader that goes away fails writes instead of killing glov before it can undo anything.
final class OnInterrupt: @unchecked Sendable {
    private static let signals = [SIGINT, SIGTERM, SIGHUP]
    private var sources: [DispatchSourceSignal] = []

    init(_ undo: @escaping @Sendable () async -> Void) {
        signal(SIGPIPE, SIG_IGN)
        for sig in Self.signals {
            signal(sig, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: sig, queue: .global())
            source.setEventHandler {
                // A dispatch thread, not the cooperative pool, so waiting here cannot starve the Task.
                let done = DispatchSemaphore(value: 0)
                Task { await undo(); done.signal() }
                _ = done.wait(timeout: .now() + 10)
                exit(128 + sig)
            }
            source.resume()
            sources.append(source)
        }
    }

    func cancel() {
        sources.forEach { $0.cancel() }
        for sig in Self.signals + [SIGPIPE] { signal(sig, SIG_DFL) }
    }
}
