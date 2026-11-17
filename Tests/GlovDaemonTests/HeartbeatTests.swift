import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI

/// The heartbeat at test speed: `guest.info` every 100 ms, 60 ms to answer, two misses in a row make the Guest Service unresponsive.
private func channel(
    _ path: String, interval: Duration = .milliseconds(100), timeout: Duration = .milliseconds(60), log: @escaping @Sendable (String) -> Void = { _ in }
) -> GuestChannel {
    GuestChannel(
        socketPath: path, retryInterval: .milliseconds(20), probeInterval: .milliseconds(100),
        heartbeatInterval: interval, heartbeatTimeout: timeout, heartbeatMisses: 2, log: log)
}

/// The lines a channel logged.
private final class LogLines: @unchecked Sendable {
    private let lock = NSLock()
    private var lines: [String] = []
    func append(_ line: String) { lock.withLock { lines.append(line) } }
    func contains(_ text: String) -> Bool { lock.withLock { lines.contains { $0.contains(text) } } }
}

/// A `VMGuestService` on a `FakeGuestService`, as the daemon runs it for a QEMU VM but with the fast heartbeat.
private struct Rig {
    let dir: String
    let fake: FakeGuestService
    let agent: VMGuestService
    let log = LogLines()

    init(interval: Duration = .milliseconds(100), timeout: Duration = .milliseconds(60)) async throws {
        dir = try shortTempDir()
        let config = VMConfig(name: "h", directory: dir, firmwareCode: "/fw", tpm: false)
        fake = try FakeGuestService(socketPath: config.guestSocketPath)
        agent = VMGuestService(config: config, channel: channel(config.guestSocketPath, interval: interval, timeout: timeout, log: log.append))
        await agent.start()
        await eventually { await agent.isReady }
    }

    func cleanup() async {
        await agent.stop()
        fake.stop()
        try? FileManager.default.removeItem(atPath: dir)
    }
}

@Suite struct HeartbeatTests {
    @Test func missedRepliesMakeTheGuestServiceUnresponsiveKeepingWhatItIs() async throws {
        let r = try await Rig()
        r.fake.answering = false
        await eventually { await r.agent.status().state == "unresponsive" }
        let s = await r.agent.status()
        #expect(s.os == "windows" && s.version == "0.0-fake")
        #expect(s.user == nil && s.bootId == nil)
        #expect(s.problem?.contains("no reply from the Guest Service") == true, "\(s.problem ?? "no problem")")
        #expect(await r.agent.isReady == false)
        await r.cleanup()
    }

    @Test func requestsWhileUnresponsiveFailAtOnce() async throws {
        let r = try await Rig()
        r.fake.answering = false
        await eventually { await r.agent.status().state == "unresponsive" }
        let start = ContinuousClock.now
        await #expect {
            try await r.agent.execStart(Params.Exec(vm: "h", argv: ["x"]))
        } throws: { "\($0)".contains("Guest Service not responding") }
        #expect(ContinuousClock.now - start < .milliseconds(50))
        await r.cleanup()
    }

    @Test func recoveringFromTheSameBootKeepsJobsAndTheSession() async throws {
        let r = try await Rig()
        r.fake.script = { _ in [.hold] }
        let job = try await r.agent.execStart(Params.Exec(vm: "h", argv: ["long"])).jobId
        let pid = 100  // the fake's first pid
        let events = await r.agent.channel.events()
        r.fake.answering = false
        await eventually { await r.agent.status().state == "unresponsive" }
        r.fake.answering = true
        await eventually { await r.agent.status().state == "ready" }
        #expect(await r.agent.status().bootId == "boot-1")
        r.fake.notify("exec.output", ["pid": .int(pid), "stream": "stdout", "data": .string(Data("still here\n".utf8).base64EncodedString())])
        r.fake.notify("exec.exit", ["pid": .int(pid), "code": 0])
        await eventually { (try? await r.agent.execRead(Params.ExecRead(vm: "h", jobId: job)))?.exited == true }
        let read = try await r.agent.execRead(Params.ExecRead(vm: "h", jobId: job))
        #expect(!read.lost && read.code == 0 && read.data == "still here\n")
        await r.agent.channel.stop()
        var sessions = 0
        for await e in events { if case .session = e { sessions += 1 } }
        #expect(sessions == 0)
        await r.cleanup()
    }

    @Test func recoveringIntoANewBootMarksJobsLost() async throws {
        let r = try await Rig()
        r.fake.script = { _ in [.hold] }
        let job = try await r.agent.execStart(Params.Exec(vm: "h", argv: ["long"])).jobId
        r.fake.answering = false
        await eventually { await r.agent.status().state == "unresponsive" }
        r.fake.bootId = "boot-2"
        r.fake.answering = true
        await eventually { await r.agent.status().bootId == "boot-2" }
        await eventually { (try? await r.agent.execRead(Params.ExecRead(vm: "h", jobId: job)))?.lost == true }
        await r.cleanup()
    }

    @Test func aRebootedGuestAnnouncingItselfWhileUnresponsiveIsANewSession() async throws {
        // virtio-serial: no EOF when the guest reboots, only the new Guest Service's guest.ready on the same connection.
        let r = try await Rig()
        r.fake.answering = false
        await eventually { await r.agent.status().state == "unresponsive" }
        r.fake.bootId = "boot-2"
        r.fake.sendReady()  // while it does not answer yet: the old session is over, the new one not there
        await eventually { await r.agent.status().state == "connecting" }
        r.fake.answering = true
        r.fake.sendReady()
        await eventually { await r.agent.status().bootId == "boot-2" }
        #expect(await r.agent.status().state == "ready")
        await r.cleanup()
    }

    @Test func aUserChangeShowsUp() async throws {
        let r = try await Rig()
        #expect(await r.agent.status().user == "tester")
        r.fake.user = nil
        await eventually { await r.agent.status().user == nil }
        #expect(await r.agent.status().state == "ready")
        r.fake.user = "someone"
        await eventually { await r.agent.status().user == "someone" }
        await r.cleanup()
    }

    @Test func anyFrameResetsTheMissCount() async throws {
        // Heartbeats wait 20 ms every 200 ms, so output every 100 ms mostly arrives between them, yet always
        // between two of them: guest.info goes unanswered (a busy agent), but no two misses are ever in a row.
        let r = try await Rig(interval: .milliseconds(200), timeout: .milliseconds(20))
        r.fake.answering = false
        let deadline = ContinuousClock.now + .milliseconds(1200)
        while ContinuousClock.now < deadline {
            r.fake.notify("exec.output", ["pid": 1, "stream": "stdout", "data": "eA=="])
            try await Task.sleep(for: .milliseconds(100))
            #expect(await r.agent.status().state == "ready")
        }
        await r.cleanup()
    }

    /// What the daemon log needs to tell a lost answer from a silent guest.
    @Test func sessionChangesAndDroppedFramesAreLogged() async throws {
        let r = try await Rig()
        await eventually { r.log.contains("Guest Service session: windows 0.0-fake, boot boot-1") }
        r.fake.answering = false
        await eventually { r.log.contains("Guest Service stopped answering: 2 heartbeats in a row") }
        r.fake.answering = true
        await eventually { r.log.contains("Guest Service answers again") }

        var n = UInt32(5).bigEndian
        r.fake.rawWrite(Data(bytes: &n, count: 4) + Data("nope!".utf8))
        await eventually { r.log.contains("dropped a 5-byte frame from the Guest Service that is not JSON") }
        await #expect(throws: GuestServiceError.self) {
            try await r.agent.channel.request("test.delay", ["ms": 300], timeout: .milliseconds(50))
        }
        await eventually { r.log.contains("-byte answer to test.delay (request ") && r.log.contains("which no longer waits for it") }
        #expect(!r.log.contains("answer to guest.info"))
        #expect(await r.agent.status().state == "ready")

        r.fake.dropConnection()
        await eventually { r.log.contains("Guest Service connection closed") }
        await r.cleanup()
    }
}
