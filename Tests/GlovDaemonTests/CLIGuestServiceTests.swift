import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

/// `glov exec` and `glov cp` as child processes against an in-process daemon with a `FakeGuestService`.
@Suite struct CLIGuestServiceTests {
    private func glov(_ f: GuestServiceFixture, _ arguments: [String]) async throws -> (status: Int32, out: String, err: String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: try builtBinary("glov"))
        p.arguments = arguments
        p.environment = ProcessInfo.processInfo.environment.merging(["GLOV_HOME": f.home.path]) { $1 }
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        // A termination handler, not waitUntilExit: that blocks a cooperative thread, and with other tests spawning
        // processes at the same time it was seen to miss the exit and hang the whole run.
        let (exited, exit) = AsyncStream<Void>.makeStream()
        p.terminationHandler = { _ in exit.yield(); exit.finish() }
        try p.run()
        // Read both pipes while the child runs, so a full pipe cannot stall it; off the cooperative pool.
        async let o = Blocking.run { out.fileHandleForReading.readDataToEndOfFile() }
        async let e = Blocking.run { err.fileHandleForReading.readDataToEndOfFile() }
        let (od, ed) = await (o, e)
        for await _ in exited {}
        return (p.terminationStatus, String(decoding: od, as: UTF8.self), String(decoding: ed, as: UTF8.self))
    }

    @Test(.timeLimit(.minutes(1)))
    func vmWaitSaysWhatTheInstallIsDoingAndStatusShowsIt() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        var config = try f.home.loadConfig("win11")
        config.install = .init(drives: [], started: Date().addingTimeInterval(-16 * 60))
        try f.home.update("win11") { [c = config] config in config = c }
        let status = try await glov(f, ["vm", "status", "win11"])
        #expect(status.out.contains("install: first sign-in and setup script, 16m"), "\(status.out)")
        #expect(status.out.contains("longer than usual; see the screen with `glov vm screenshot win11`"))

        // The install finishes while the wait is under way: one progress line, then the finished state.
        let finish = Task {
            try await Task.sleep(for: .seconds(6))
            try f.home.update("win11") { [c = config.finishingInstall()] config in config = c }
        }
        let wait = try await glov(f, ["vm", "wait", "win11", "guest-ready", "--timeout", "30"])
        try await finish.value
        #expect(wait.status == 0, "\(wait.err)")
        #expect(wait.err.contains("waiting for win11 to be guest-ready"), "\(wait.err)")
        #expect(wait.err.contains("win11: installing Windows: first sign-in and setup script; longer than usual"), "\(wait.err)")
        #expect(wait.out.contains("Guest Service ready") && !wait.out.contains("install:"), "\(wait.out)")
    }

    @Test(.timeLimit(.minutes(1)))
    func guestServiceCommandsUseTheDaemonBundleAndStatusExplainsUnknown() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        let help = try await glov(f, ["vm", "guest", "--help"])
        #expect(help.status == 0 && help.out.contains("Guest Service"))
        let old = try await glov(f, ["agent", "check", "win11"])
        #expect(old.status != 0)
        let oldFlag = try await glov(f, ["vm", "guest", "check", "win11", "--agent", "missing.exe"])
        #expect(oldFlag.status != 0)
        let oldWait = try await glov(f, ["vm", "wait", "win11", "agent-ready", "--timeout", "0.01"])
        #expect(oldWait.status != 0 && oldWait.err.contains("guest-ready"))
        let checked = try await glov(f, ["vm", "guest", "check", "win11"])
        #expect(checked.status == 0 && checked.out.contains("unknown") && checked.out.contains("tools/build.sh"))
        let status = try await glov(f, ["vm", "status", "win11"])
        #expect(status.status == 0 && status.out.contains("0.0-fake") && status.out.contains("unknown"))
        let updated = try await glov(f, ["vm", "guest", "update", "win11"])
        #expect(updated.status != 0 && updated.err.contains("--guest-service <path>"))
        let custom = try await glov(f, ["vm", "guest", "check", "win11", "--guest-service", f.shared + "/missing.exe"])
        #expect(custom.status == 0 && custom.out.contains("missing.exe") && custom.out.contains("unknown"))
        #expect(f.fake.requests("file.write").isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func rebootPrintsTheStatusWithTheNewBoot() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        f.fake.rebootsAfterAnswering(to: "boot-2")
        let r = try await glov(f, ["vm", "reboot", "win11", "--timeout", "10"])
        #expect(r.status == 0 && r.out.hasPrefix("win11: running") && r.out.contains("Guest Service ready") && r.out.contains("boot boot-2"), "\(r.out)\(r.err)")
    }

    @Test(.timeLimit(.minutes(1)))
    func execPrintsTheOutputAndExitsWithTheGuestExitCode() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.script = { _ in [.out("stdout", "out\n"), .out("stderr", "err\n"), .exit(7)] }
        let r = try await glov(f, ["exec", "win11", "--", "cmd.exe", "/c", "exit", "7"])
        #expect(r.out == "out\nerr\n" && r.status == 7)
        let start = try #require(f.fake.requests("exec.start").first)
        #expect(start["argv"] == ["cmd.exe", "/c", "exit", "7"])
        f.fake.script = { _ in [.exit(0)] }
        #expect(try await glov(f, ["exec", "win11", "--", "true"]).status == 0)
    }

    @Test(.timeLimit(.minutes(1)))
    func execAsUserWithNobodySignedInExitsWithTheNoUserCode() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.user = nil
        let noUser = try await glov(f, ["exec", "win11", "--as", "user", "--", "cmd.exe", "/c", "exit", "0"])
        #expect(noUser.status == 75 && noUser.err.contains("no user is signed in"))
        // Only that guest error has its own code; a guest command may exit 75 itself, other failures stay 1.
        f.fake.script = { _ in [.exit(75)] }
        #expect(try await glov(f, ["exec", "win11", "--", "x"]).status == 75)
        let missing = try await glov(f, ["exec", "nope", "--", "x"])
        #expect(missing.status == 1)
        f.fake.user = "tester"
        f.fake.script = { _ in [.out("stdout", "ok\n"), .exit(0)] }
        #expect(try await glov(f, ["exec", "win11", "--as", "user", "--", "x"]).status == 0)
    }

    @Test(.timeLimit(.minutes(1)))
    func cpCopiesBothWaysAndExplainsRefusals() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        try FileManager.default.createDirectory(atPath: f.shared, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o755])
        try Data("data".utf8).write(to: URL(fileURLWithPath: f.shared + "/in.txt"))
        let up = try await glov(f, ["cp", f.shared + "/in.txt", "win11:C:\\w\\in.txt"])
        #expect(up.status == 0 && up.out == "1 files, 4 bytes\n")
        #expect(f.fake.files["C:\\w\\in.txt"] == Data("data".utf8))
        let down = try await glov(f, ["cp", "win11:C:\\w\\in.txt", f.shared + "/back.txt"])
        #expect(down.status == 0)
        #expect(try String(contentsOfFile: f.shared + "/back.txt", encoding: .utf8) == "data")
        let bad = try await glov(f, ["cp", "/etc/hosts", "win11:C:\\h"])
        #expect(bad.status != 0 && bad.err.contains("allowlist"))
    }
}
