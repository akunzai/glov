import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemonAPI

@Suite struct ExecTests {
    @Test func runReturnsTheExitCodeATailAndTheLogPath() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.script = { _ in [.out("stdout", "hello\r\n"), .out("stderr", "oops\n"), .exit(3)] }
        let r = try await f.run(["cmd.exe", "/c", "x"])
        #expect(r.exited && r.code == 3 && !r.lost)
        #expect(r.tail == "hello\noops\n")
        #expect(r.lineCount == 2)
        #expect(r.logPath == f.config.directory + "/jobs/\(r.jobId).log")
        #expect(r.jobId == "j1")
        // The log is the raw interleaved output.
        #expect(try String(contentsOfFile: r.logPath, encoding: .utf8) == "hello\r\noops\n")
        let start = try #require(f.fake.requests("exec.start").first)
        #expect(start["argv"] == ["cmd.exe", "/c", "x"] && start["stdin"] == false)
    }

    @Test func parametersReachTheGuestServiceAndStdinIsDelivered() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let r = try await f.client.call(
            Method.execRun,
            Params.Exec(vm: "win11", argv: ["sort"], cwd: "C:\\w", env: ["K": "V"], as: "user", stdin: "b\na\n"), as: Results.ExecRun.self)
        let start = try #require(f.fake.requests("exec.start").first)
        #expect(start["cwd"] == "C:\\w" && start["env"] == ["K": "V"] && start["as"] == "user" && start["stdin"] == true)
        let stdin = try #require(f.fake.requests("exec.stdin").first)
        #expect(stdin["eof"] == true)
        #expect(f.fake.stdin(try #require(stdin["pid"]).intValue) == Data("b\na\n".utf8))
        #expect(r.exited)
    }

    @Test func asUserWithNobodySignedInFailsWithTheNoUserCodeAndKeepsTheMessage() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.user = nil
        let e = try #require(await f.failure(Method.execStart, ["vm": "win11", "argv": ["x"], "as": "user"]))
        #expect(e.code == RPCError.noUser && e.message == "Guest Service error 4: no user is signed in on the console")
        let other = try #require(await f.failure(Method.execStart, ["vm": "win11", "argv": []]))
        #expect(other.code != RPCError.noUser)
    }

    @Test func exitCountsAfterAllOutputEvenWhenItArrivesImmediately() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.script = { _ in (0..<300).map { .out($0 % 2 == 0 ? "stdout" : "stderr", "line \($0)\n") } + [.exit(0)] }
        let r = try await f.run(["many"])
        #expect(r.exited && r.lineCount == 300)
        #expect(r.tail.hasSuffix("line 299\n") && r.tail.hasPrefix("line 250\n"))
        #expect(r.tail.split(separator: "\n").count == 50)
    }

    @Test func outputThatBeatsTheStartResponseIsNotLost() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.outputBeforeStartResponse = true
        f.fake.script = { _ in [.out("stdout", "early\n"), .out("stdout", "late\n"), .exit(0)] }
        let r = try await f.run(["race"])
        #expect(r.tail == "early\nlate\n")
    }

    @Test func aRunPastItsTimeoutKeepsTheJobAndItCanBeReadAndKilled() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.script = { _ in [.out("stdout", "building\n"), .hold] }
        let r = try await f.run(["msbuild"], timeout: 0.3)
        #expect(!r.exited && r.code == nil && !r.lost && r.tail == "building\n")
        let first = try await f.read(r.jobId)
        #expect(first.data == "building\n" && !first.exited && first.cursor == 9)
        _ = try await f.client.call(Method.execKill, Params.ExecKill(vm: "win11", jobId: r.jobId), as: Results.OK.self)
        await eventually { (try? await f.read(r.jobId, cursor: first.cursor))?.exited == true }
        let done = try await f.read(r.jobId, cursor: first.cursor)
        #expect(done.code == 137 && done.data == "")
        let again = await f.failure(Method.execKill, ["vm": "win11", "jobId": .string(r.jobId)])
        #expect(again?.message.contains("already exited with code 137") == true)
    }

    @Test func startReturnsAJobIdAtOnce() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.script = { _ in [.hold] }
        let started = try await f.client.call(Method.execStart, Params.Exec(vm: "win11", argv: ["long"]), as: Results.JobStarted.self)
        #expect(started.jobId == "j1")
        let second = try await f.client.call(Method.execStart, Params.Exec(vm: "win11", argv: ["long"]), as: Results.JobStarted.self)
        #expect(second.jobId == "j2")
    }

    @Test func theRingKeepsOneMiBWhileTheLogKeepsEverythingAndCursorsPage() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let piece = 65_537
        f.fake.script = { _ in
            (0..<48).map { .out("stdout", String(format: "%06d", $0) + String(repeating: "a", count: piece - 7) + "\n") } + [.exit(0)]
        }
        let r = try await f.run(["big"])
        let total = 48 * piece
        let attrs = try FileManager.default.attributesOfItem(atPath: r.logPath)
        #expect(attrs[.size] as? Int == total)
        #expect(try String(contentsOfFile: r.logPath, encoding: .utf8).hasPrefix("000000"))

        let head = try await f.read(r.jobId, maxBytes: 1000)
        #expect(head.dropped, "the first bytes fell out of the 1 MiB window")
        #expect(head.cursor == total - (1 << 20) + 1000)
        #expect(head.logPath == r.logPath)

        let end = try await f.read(r.jobId, cursor: total - 5)
        #expect(end.data == "aaaa\n" && end.cursor == total && end.exited && !end.dropped)

        // Paging from inside the window reaches the end exactly.
        var cursor = total - 300_000
        var bytes = 0
        while true {
            let page = try await f.read(r.jobId, cursor: cursor, maxBytes: 100_000)
            #expect(!page.dropped)
            bytes += page.data.utf8.count
            if page.cursor == cursor { break }
            cursor = page.cursor
        }
        #expect(cursor == total && bytes == 300_000)
    }

    @Test func aMultibyteCharacterIsNeverSplitAcrossReads() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.script = { _ in [.out("stdout", "日本語"), .exit(0)] }
        let r = try await f.run(["u"])
        let one = try await f.read(r.jobId, maxBytes: 4)
        #expect(one.data == "日" && one.cursor == 3)
        let rest = try await f.read(r.jobId, cursor: one.cursor)
        #expect(rest.data == "本語" && rest.cursor == 9)
    }

    @Test func onlyTheLast50FinishedJobsAreKept() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        var first: Results.ExecRun?
        for i in 0..<52 {
            let r = try await f.run(["n"])
            if i == 0 { first = r }
        }
        let gone = await f.failure(Method.execRead, ["vm": "win11", "jobId": "j2"])
        #expect(gone?.message.contains("no job") == true)
        _ = try await f.read("j3")
        _ = try await f.read("j52")
        #expect(!FileManager.default.fileExists(atPath: try #require(first).logPath))
    }

    @Test func aReconnectMarksRunningJobsLost() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let done = try await f.run(["quick"])  // finished before the reconnect: stays finished
        f.fake.script = { _ in [.out("stdout", "x\n"), .hold] }
        let r = try await f.run(["w"], timeout: 0.2)
        f.fake.script = { _ in [.exit(0)] }
        f.fake.bootId = "boot-2"
        f.fake.dropConnection()
        await eventually { (try? await f.read(r.jobId))?.lost == true }
        let lost = try await f.read(r.jobId)
        #expect(lost.lost && !lost.exited && lost.code == nil)
        #expect(try await f.read(done.jobId).lost == false)
        let kill = await f.failure(Method.execKill, ["vm": "win11", "jobId": .string(r.jobId)])
        #expect(kill?.message.contains("lost") == true)
        // The new session works.
        await eventually { (try? await f.run(["again"]))?.exited == true }
    }

    @Test func retryingWhileNoUserWaitsForTheSignInAndNothingElse() async throws {
        let noUser = RPCError(code: RPCError.noUser, message: "no user is signed in")
        var calls = 0, waits = 0
        let r = try await retryingWhileNoUser(timeout: .seconds(10), interval: .milliseconds(1), waiting: { waits += 1 }) {
            calls += 1
            if calls < 3 { throw noUser }
            return calls
        }
        #expect(r == 3 && waits == 1)

        calls = 0
        await #expect(throws: RPCError(code: RPCError.failed, message: "VM not running")) {
            try await retryingWhileNoUser(timeout: .seconds(10), interval: .milliseconds(1)) {
                calls += 1
                throw RPCError(code: RPCError.failed, message: "VM not running")
            }
        }
        #expect(calls == 1)

        await #expect(throws: noUser) {
            try await retryingWhileNoUser(timeout: .milliseconds(20), interval: .milliseconds(5)) { () async throws -> Int in throw noUser }
        }
    }
}
