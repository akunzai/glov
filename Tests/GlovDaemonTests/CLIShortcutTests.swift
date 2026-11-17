import Foundation
import GlovCore
import GlovDaemonAPI
import Testing

@Suite struct CLIShortcutTests {
    @Test(.timeLimit(.minutes(1)), arguments: ["glov"])
    func helpUsesTheInvokedName(executable: String) async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let help = try await runglov(["--help"], home: home, executable: executable)
        #expect(help.status == 0 && help.out.contains("USAGE: \(executable) ") && help.out.contains("<subcommand>"))
        for group in ["vm", "share", "snapshot"] {
            let help = try await runglov([group, "--help"], home: home, executable: executable)
            #expect(help.status == 0 && help.out.contains("ls") && help.out.contains("rm"), "\(group): \(help.out)")
        }
        let viewer = try await runglov(["view", "--help"], home: home, executable: executable)
        #expect(viewer.status == 0 && viewer.out.contains("--read-only"))
    }

    @Test(.timeLimit(.minutes(1)), arguments: ["glov"])
    func shortcutsUseExistingHandlersAndDeletionChecks(executable: String) async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let firmware = home.path + "/code.fd"
        try Data(count: 4096).write(to: URL(fileURLWithPath: firmware))
        try home.create(VMConfig(name: "win11", directory: home.directory(of: "win11"), firmwareCode: firmware, tpm: false))
        let daemon = try DaemonProcess(home: home)
        defer { daemon.process.terminate() }
        try await daemon.waitUntilReady()

        let list = try await runglov(["vm", "ls"], home: home, executable: executable)
        #expect(list.status == 0 && list.out.contains("win11"), "\(list.err)")
        let shares = try await runglov(["share", "ls", "win11"], home: home, executable: executable)
        #expect(shares.status == 0 && shares.out == "no shares\n", "\(shares.err)")
        // Snapshot operations may need qemu-img; either outcome must match the full command.
        for (full, short) in [(["snapshot", "list", "win11"], ["snapshot", "ls", "win11"]),
                              (["snapshot", "delete", "win11", "missing"], ["snapshot", "rm", "win11", "missing"]),
                              (["share", "remove", "win11", "missing"], ["share", "rm", "win11", "missing"])] {
            let expected = try await runglov(full, home: home, executable: executable)
            let actual = try await runglov(short, home: home, executable: executable)
            #expect(actual.status == expected.status && actual.out == expected.out && actual.err == expected.err, "\(short): \(actual.err)")
        }
        let declined = try await runglov(["vm", "rm", "win11"], home: home, stdin: "n\n", executable: executable)
        #expect(declined.status != 0 && declined.err.contains("This cannot be undone. [y/N]"))
        #expect(FileManager.default.fileExists(atPath: home.directory(of: "win11")))
        let prefix = try await runglov(["vm", "rm", "win", "--yes"], home: home, executable: executable)
        #expect(prefix.status != 0 && prefix.err.hasPrefix("\(executable): ") && home.vmNames() == ["win11"])
        let removed = try await runglov(["vm", "rm", "win11", "--yes"], home: home, executable: executable)
        #expect(removed.status == 0 && home.vmNames().isEmpty, "\(removed.err)")
    }
}
