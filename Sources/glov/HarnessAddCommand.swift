import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

extension HarnessCommand {
    struct Add: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Add a harness to a running isolated VM created with --harness, and install it.",
            discussion: """
                Sets the guest up as vm create --harness would have: the harness's environment in /etc/environment, its \
                files in the guest user's home (a file already there is kept), its secret links and, for codex, \
                unprivileged user namespaces. Allows its runtime hosts, adds it to vm.json, then installs it as glov vm \
                harness install --harness does. Its secrets still need glov secret grant (glov claude, codex and agy \
                grant the ones you have stored).
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Harness: \(HarnessProfile.names).") var harness: String
        @Flag(name: .customLong("no-host-config"), help: "Do not copy your harness config from this Mac into the VM.") var noHostConfig = false

        func run() async throws {
            try await report {
                let home = GlovHome.resolve()
                let name = try home.resolve(vm)
                let config = try home.loadConfig(name)
                guard config.isolated else { throw Failure("VM \(name) is not isolated: a harness's key placeholder is replaced only by the egress proxy of an isolated VM") }
                let profile = try HarnessProfile.named(harness)
                if let refusal = HarnessAddition.refusal(vm: name, harnesses: config.harnesses, harness: profile.name) { throw Failure(refusal) }
                var names = config.harnesses ?? []
                guard !names.contains(profile.name) else {
                    say("\(name) already has \(profile.name)")
                    return
                }
                let client = daemonClient()
                let info = try await client.call(Method.vmStatus, Params.VM(vm: name), as: Results.VMInfo.self)
                guard info.state == "running", info.agent?.state == "ready" else {
                    throw Failure("\(name) must be running with its Guest Service ready: glov vm start \(name), then glov vm wait \(name) guest-ready")
                }
                names.append(profile.name)
                let profiles = try names.map { try HarnessProfile.named($0) }
                let subscribed = Set(config.egress?.subscriptions ?? [])

                say("setting \(name) up for \(profile.title)")
                let environment = try await client.call(Method.fileRead, Params.FileRead(vm: name, path: "/etc/environment"), as: Results.FileRead.self).text
                let wanted = HarnessMode.environment(environment, profiles: profiles, subscribed: subscribed, vm: name)
                if wanted != environment {
                    _ = try await client.call(Method.fileWrite, Params.FileWrite(vm: name, path: "/etc/environment", text: wanted), as: Results.FileWrite.self)
                }
                if profile.userNamespaces {
                    _ = try await client.call(Method.fileWrite, Params.FileWrite(vm: name, path: CloudInitSeed.userNamespacesPath, text: HarnessAddition.userNamespacesSetting), as: Results.FileWrite.self)
                    try await expect(client, name, CloudInitSeed.applyUserNamespaces, as: "system", "applying \(CloudInitSeed.userNamespacesPath)")
                }
                for file in profile.guestFiles {
                    let r = try await exec(client, name, ["/bin/sh", "-c", HarnessAddition.writeFileScript, "sh", file.path], as: "user", stdin: file.content)
                    if r == 10 { say("\(name): kept ~/\(file.path), which is there already") } else if r != 0 { throw Failure("writing ~/\(file.path) failed with exit code \(r)") }
                }
                for link in profile.secretLinks {
                    try await expect(client, name, ["/bin/sh", "-c", HarnessAddition.linkScript, "sh", link.path, link.target], as: "user", "linking ~/\(link.path)")
                }

                let allowed = try await client.call(Method.egressList, Params.Egress(vm: name), as: Results.Egress.self).allow
                let runtime = profile.runtime.filter { !allowed.contains($0) }
                if !runtime.isEmpty {
                    _ = try await client.call(Method.egressAllow, Params.Egress(vm: name, hosts: runtime), as: Results.Egress.self)
                }
                _ = try home.update(name) { $0.harnesses = names }
                say("\(name): added \(profile.name)\(runtime.isEmpty ? "" : ", allowed \(runtime.joined(separator: ", "))")")
                for line in profile.secretNeeds(vm: name, defined: nil) { say(line) }
                if let egress = config.egress, egress.open == true {
                    let withheld = profile.secrets.filter { $0.delivery == .tmpfs }.map(\.name)
                        .filter { (egress.secrets ?? []).contains($0) && !(egress.plainSecrets ?? []).contains($0) }
                    if !withheld.isEmpty {
                        say("\(name) is open, so \(withheld.joined(separator: ", ")) stays out of its tmpfs: any host could receive the real value; "
                            + "glov vm configure \(name) --egress open --allow-plain-secrets lets it in")
                    }
                }

                try await Install.parse([name, "--harness", profile.name] + (noHostConfig ? ["--no-host-config"] : [])).run()
            }
        }

        private func expect(_ client: DaemonClient, _ vm: String, _ argv: [String], as runAs: String, _ what: String) async throws {
            let code = try await exec(client, vm, argv, as: runAs)
            guard code == 0 else { throw Failure("\(what) failed with exit code \(code)") }
        }

        private func exec(_ client: DaemonClient, _ vm: String, _ argv: [String], as runAs: String, stdin: String? = nil) async throws -> Int {
            try await retryingWhileNoUser(timeout: Install.signInTimeout, waiting: { say("\(vm): waiting for the console sign-in") }) {
                let r = try await client.call(Method.execRun, Params.Exec(vm: vm, argv: argv, as: runAs, stdin: stdin, timeout: 30), as: Results.ExecRun.self)
                guard r.exited, let code = r.code else { throw Failure("\(argv.first ?? "") did not finish in the guest") }
                return code
            }
        }

        private func say(_ line: String) { try? FileHandle.standardOutput.write(contentsOf: Data((line + "\n").utf8)) }
    }
}
