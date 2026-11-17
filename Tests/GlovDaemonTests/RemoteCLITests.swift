import Foundation
import GlovCore
import GlovDaemonAPI
import Testing

@Suite struct RemoteCLITests {
    @Test(.enabled(if: ProcessInfo.processInfo.environment["GLOV_TEST_SSH_HOST"] != nil), .timeLimit(.minutes(1)))
    func remoteListingAndMCPReachAnIsolatedDaemonOverSystemSSH() async throws {
        let host = try #require(ProcessInfo.processInfo.environment["GLOV_TEST_SSH_HOST"])
        let f = try await GuestServiceFixture()
        let local = GlovHome(path: try shortTempDir())
        defer { f.cleanup(); try? FileManager.default.removeItem(atPath: local.path) }
        let result = try await runglov(["--remote", host, "--remote-home", f.home.path, "vm", "ls"], home: local, executable: "glov")
        #expect(result.status == 0 && result.out.contains("win11"), "\(result.err)")
        let prefix = ["--remote", host, "--remote-home", f.home.path]
        f.fake.script = { _ in [.out("stdout", "remote fixture\n"), .exit(7)] }
        let execution = try await runglov(prefix + ["exec", "win11", "--", "fixture"], home: local, executable: "glov")
        #expect(execution.status == 7 && execution.out == "remote fixture\n" && execution.err.contains("remote job:"))
        try FileManager.default.createDirectory(atPath: f.shared, withIntermediateDirectories: true)
        try Data("remote copy".utf8).write(to: URL(fileURLWithPath: f.shared + "/input"))
        let upload = try await runglov(prefix + ["cp", f.shared + "/input", "win11:C:\\fixture"], home: local, executable: "glov")
        let download = try await runglov(prefix + ["cp", "win11:C:\\fixture", f.shared + "/output"], home: local, executable: "glov")
        #expect(upload.status == 0 && download.status == 0, "\(upload.err) \(download.err)")
        #expect(try String(contentsOfFile: f.shared + "/output", encoding: .utf8) == "remote copy")
        let session = try MCPSession(home: local, executable: "glov", arguments: ["--remote", host, "--remote-home", f.home.path, "mcp"])
        defer { session.terminate() }
        _ = try await session.initialize()
        let response = try await session.request("tools/call", ["name": "vm_list", "arguments": [String: Any]()])
        let content = try #require(response["content"] as? [[String: Any]])
        #expect(content.contains { ($0["text"] as? String)?.contains("win11") == true })
        #expect(local.vmNames().isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func remoteCopiesRejectLaptopRelativePathsBeforeConnecting() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        for arguments in [["cp", "./local", "win11:C:\\work"], ["cp", "win11:C:\\work", "~/local"]] {
            let result = try await runglov(["--remote", "fixture.invalid"] + arguments, home: home, executable: "glov")
            #expect(result.status != 0 && result.err.contains("absolute path on the remote Mac"), "\(result.err)")
        }
    }

    @Test(.timeLimit(.minutes(1)))
    func remoteModeRejectsLocalConfigurationChangesBeforeConnecting() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let result = try await runglov(["--remote", "fixture.invalid", "vm", "create", "example"], home: home, executable: "glov")
        #expect(result.status != 0 && result.err.contains("not supported in remote mode"), "\(result.err)")
        #expect(home.vmNames().isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func literalHelpAfterTheTerminatorCannotBypassRemoteRestrictions() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        for command in [["share", "list"], ["ssh", "fixture"], ["vm", "rm"], ["host", "uninstall"]] {
            let result = try await runglov(["--remote", "fixture.invalid"] + command + ["--", "--help"], home: home, executable: "glov")
            #expect(result.status != 0 && result.err.contains("not supported in remote mode"), "\(result.err)")
        }
    }

    @Test(.timeLimit(.minutes(1)))
    func remoteModeAllowsShareStatus() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let result = try await runglov(["--remote", "fixture.invalid", "share", "status", "win11"], home: home, executable: "glov")
        #expect(!result.err.contains("not supported in remote mode"), "\(result.err)")
    }
}
