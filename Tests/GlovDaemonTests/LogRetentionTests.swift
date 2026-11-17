import Foundation
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// The daemon's own log and exec job logs stay bounded.
@Suite struct LogRetentionTests {
    @Test func theDaemonLogIsRotatedOnceOnItsDescriptorsPastTheCap() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let path = dir + "/glovd.log"
        let out = open(path, O_WRONLY | O_CREAT | O_APPEND, 0o600)
        let err = dup(out)
        defer { close(out); close(err) }
        func write(_ fd: Int32, _ s: String) { _ = s.withCString { Darwin.write(fd, $0, strlen($0)) } }

        write(out, "0123")
        #expect(!LogRotation.rotateIfNeeded(fds: [out, err], cap: 8), "under the cap")
        write(err, "4567")
        #expect(LogRotation.rotateIfNeeded(fds: [out, err], cap: 8))
        write(out, "out ")
        write(err, "err")
        #expect(try String(contentsOfFile: path + ".1", encoding: .utf8) == "01234567")
        #expect(try String(contentsOfFile: path, encoding: .utf8) == "out err", "both descriptors now write to the fresh file")

        write(out, " more")
        #expect(LogRotation.rotateIfNeeded(fds: [out, err], cap: 8))
        #expect(try String(contentsOfFile: path + ".1", encoding: .utf8) == "out err more", "one old file is kept")
    }

    @Test func aPipeIsLeftAlone() {
        var fds: [Int32] = [0, 0]
        #expect(pipe(&fds) == 0)
        defer { close(fds[0]); close(fds[1]) }
        #expect(!LogRotation.rotateIfNeeded(fds: [fds[1]], cap: 0))
    }

    @Test(.timeLimit(.minutes(1)))
    func jobLogsFromAnEarlierDaemonCountTowardTheFinishedJobLimit() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let jobs = f.config.jobsDirectory
        try FileManager.default.createDirectory(atPath: jobs, withIntermediateDirectories: true)
        let limit = VMGuestService.maxFinishedJobs
        for n in 1...(limit + 5) { try Data("old \(n)".utf8).write(to: URL(fileURLWithPath: jobs + "/j\(n).log")) }
        try Data().write(to: URL(fileURLWithPath: jobs + "/maintenance-left.log"))
        func logs() throws -> Set<String> { Set(try FileManager.default.contentsOfDirectory(atPath: jobs)) }

        // A new daemon (here: a new Guest Service for the VM) finds them.
        await f.dispatcher.startGuestService(f.config)
        _ = try await f.client.call(Method.vmWait, Params.Wait(vm: "win11", state: "guest-ready", timeout: 10), as: Results.VMInfo.self)
        #expect(try logs() == Set((6...(limit + 5)).map { "j\($0).log" }), "the newest \(limit) are kept, a stray maintenance log goes")

        let r = try await f.run(["cmd.exe", "/c", "x"])
        #expect(r.jobId == "j\(limit + 6)", "ids are never reused")
        #expect(try logs().count == limit && !(try logs().contains("j6.log")), "a new finished job pushes the oldest out")
    }
}
