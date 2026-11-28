import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct DaemonCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "host", abstract: "Set up this Mac: install or remove the glovd LaunchAgent, build Windows install media.",
        subcommands: [Install.self, Uninstall.self, MediaCommand.self])

    struct Install: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Copy glovd into the glov home, sign it, and load the LaunchAgent that runs it.",
            discussion: """
                Run it again after rebuilding or upgrading glov: the LaunchAgent runs <home>/bin/glovd, not the build output.

                Replacing a running daemon powers off every Linux VM (they run inside it) and cuts off \
                running exec jobs and share syncs on every VM; Windows VMs keep running. When anything would be interrupted it asks \
                first, and without a terminal it refuses unless --yes is given. Linux VMs are shut down cleanly (as vm stop does) \
                before the reload, started again afterwards and waited for (up to 300 s each), and then every idle VM whose Guest \
                Service is older than the bundled one is updated; a busy one is skipped, never forced.
                """)
        @Flag(name: .shortAndLong, help: "Do not ask before interrupting running VMs and work.") var yes = false
        @Flag(help: "Leave the Linux VMs that were running stopped.") var noRestart = false
        @Flag(help: "Do not update outdated Guest Services after the reload.") var noGuestUpdate = false
        func run() async throws {
            try await report {
                let home = GlovHome.resolve()
                let reload = DaemonReload.cli(home)
                // No daemon answering (a first install): nothing to interrupt or restart.
                let impact = try? await reload.impact()
                var outcomes: [DaemonReload.Outcome] = []
                if let impact {
                    try confirm(impact, verb: "installing", yes: yes)
                    outcomes += await reload.stop(impact.linuxVMs)
                }
                try FileManager.default.createDirectory(atPath: home.path, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
                let daemon = try await Blocking.run {
                    let source = try daemonExecutable()
                    let assets = (source as NSString).deletingLastPathComponent
                    try GuestServiceDistribution.install(from: assets, to: home.path + "/bin")
                    try DiskImageTool.install(from: assets, to: home.path + "/bin")
                    return try LaunchAgent.installExecutable(from: source, home: home)
                }
                try FileManager.default.createDirectory(
                    atPath: (LaunchAgent.plistPath as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
                try LaunchAgent.plist(executable: daemon, home: home).write(to: URL(fileURLWithPath: LaunchAgent.plistPath))
                _ = try await launchctl(["bootout", "gui/\(getuid())/\(LaunchAgent.label)"])  // reload cleanly if already loaded
                // bootout returns before launchd has let go of the service, and a bootstrap in that window fails with
                // "Bootstrap failed: 5: Input/output error"; retry for a few seconds.
                var result = try await launchctl(["bootstrap", "gui/\(getuid())", LaunchAgent.plistPath])
                for _ in 0..<20 where result.status != 0 {
                    try await Task.sleep(for: .milliseconds(250))
                    result = try await launchctl(["bootstrap", "gui/\(getuid())", LaunchAgent.plistPath])
                }
                guard result.status == 0 else { throw LaunchctlError(output: result.output) }
                // Return once the daemon answers, so a command run right after this one finds it.
                let client = DaemonClient(socketPath: home.socketPath)
                for _ in 0..<50 where (try? await client.call(Method.vmList)) == nil { try await Task.sleep(for: .milliseconds(100)) }
                print("installed \(LaunchAgent.plistPath); glovd runs \(daemon) for \(home.path)")
                if let impact, !noRestart { outcomes += await reload.restart(impact) }
                if impact != nil, !noGuestUpdate { outcomes += await reload.updateGuestServices() }
                if impact != nil { print(DaemonReload.summary(outcomes)) }
                if let note = WindowsProvisioner.missingHostToolsNote(WindowsProvisioner.missingHostTools()) {
                    FileHandle.standardError.write(Data((note + "\n").utf8))
                }
                if outcomes.contains(where: \.failed) { throw ExitCode.failure }
            }
        }
    }

    struct Uninstall: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Unload the LaunchAgent and delete its plist.",
            discussion: """
                Windows VMs keep running. Linux VMs run inside the daemon: they are shut down cleanly \
                first and stay stopped, and running exec jobs and share syncs on every VM are cut off. When anything would be \
                interrupted it asks first, and without a terminal it refuses unless --yes is given.
                """)
        @Flag(name: .shortAndLong, help: "Do not ask before interrupting running VMs and work.") var yes = false
        func run() async throws {
            try await report {
                let reload = DaemonReload.cli(GlovHome.resolve())
                var outcomes: [DaemonReload.Outcome] = []
                if let impact = try? await reload.impact() {
                    try confirm(impact, verb: "uninstalling", yes: yes)
                    outcomes = await reload.stop(impact.linuxVMs)
                }
                _ = try await launchctl(["bootout", "gui/\(getuid())/\(LaunchAgent.label)"])
                try? FileManager.default.removeItem(atPath: LaunchAgent.plistPath)
                print("uninstalled \(LaunchAgent.label)")
                if outcomes.contains(where: \.failed) { throw ExitCode.failure }
            }
        }
    }
}

extension DaemonReload {
    static func cli(_ home: GlovHome) -> DaemonReload {
        let client = DaemonClient(socketPath: home.socketPath)
        return DaemonReload(rpc: { try await client.call($0, $1) }, log: { FileHandle.standardError.write(Data(($0 + "\n").utf8)) })
    }
}

/// Throws unless the reload may go ahead: declined at the prompt, or refused without a terminal.
private func confirm(_ impact: DaemonReload.Impact, verb: String, yes: Bool) throws {
    let decision = DaemonReload.decide(impact, verb: verb, yes: yes, isTerminal: isatty(STDIN_FILENO) == 1) { question in
        FileHandle.standardError.write(Data(question.utf8))
        return readLine()
    }
    switch decision {
    case .proceed: return
    case .declined:
        FileHandle.standardError.write(Data("nothing changed\n".utf8))
        throw ExitCode.failure
    case .refused(let message): throw ProvisionError(message)
    }
}

/// `glovd` next to this binary.
private func daemonExecutable() throws -> String {
    let me = URL(fileURLWithPath: Bundle.main.executablePath ?? CommandLine.arguments[0]).resolvingSymlinksInPath()
    let path = me.deletingLastPathComponent().appendingPathComponent("glovd").path
    guard FileManager.default.isExecutableFile(atPath: path) else { throw LaunchctlError(output: "glovd not found next to \(me.path)") }
    return path
}

/// Off the cooperative pool, since it waits for launchctl to exit.
private func launchctl(_ arguments: [String]) async throws -> (status: Int32, output: String) {
    try await Blocking.run {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        p.arguments = arguments
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        try p.run()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return (p.terminationStatus, String(decoding: data, as: UTF8.self))
    }
}
