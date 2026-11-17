import Foundation
import Testing

@testable import GlovCore

/// Waits until `condition` holds (polling) or fails the test after `seconds`. The deadline is generous on purpose:
/// with one cooperative thread (`LIBDISPATCH_COOPERATIVE_POOL_STRICT`) on a 3-core CI runner, the hundreds of tests
/// starting at once hold back every test's continuations for seconds, and a passing wait never reaches it.
func eventually(_ message: Comment = "condition never held", within seconds: Double = 30, _ condition: () async -> Bool) async {
    let deadline = ContinuousClock.now + .seconds(seconds)
    while !(await condition()) {
        if ContinuousClock.now > deadline { Issue.record(message); return }
        try? await Task.sleep(for: .milliseconds(10))
    }
}

private func channel(_ path: String) -> GuestChannel {
    GuestChannel(socketPath: path, retryInterval: .milliseconds(20), probeInterval: .milliseconds(100))
}

@Suite struct GuestChannelTests {
    @Test func framesAreBigEndianLengthPlusPayload() {
        #expect(GuestFraming.encode(Data("{}".utf8)) == Data([0, 0, 0, 2, 0x7b, 0x7d]))
    }

    @Test func connectsWhenTheGuestServiceAnnouncesItself() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        defer { fake.stop() }
        let c = channel(fake.path)
        await c.start()
        defer { Task { await c.stop() } }
        await eventually { await c.isReady }
        #expect(await c.info?.bootId == "boot-1")
        #expect(await c.info?.os == "windows")
        // guest.ready is followed by an guest.info call before the session counts.
        #expect(fake.requests("guest.info").count >= 1)
    }

    @Test func retriesUntilTheSocketExistsAndProbesAGuestServiceThatAlreadyRanBeforeTheHostConnected() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let c = channel(dir + "/guest.sock")
        await c.start()
        defer { Task { await c.stop() } }
        try await Task.sleep(for: .milliseconds(150))
        #expect(await c.isReady == false)
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        fake.sendReadyOnConnect = false  // the Guest Service opened the port long ago and says nothing new
        defer { fake.stop() }
        await eventually { await c.isReady }
    }

    @Test func requestsFailBeforeAndAfterTheSession() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let c = channel(dir + "/guest.sock")
        await #expect(throws: GuestServiceError.notConnected) { try await c.request("test.delay", ["ms": 0]) }
    }

    @Test func answersArriveByIdAndNotificationsInterleave() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        defer { fake.stop() }
        let c = channel(fake.path)
        await c.start()
        defer { Task { await c.stop() } }
        await eventually { await c.isReady }
        let events = await c.events()
        async let slow = c.request("test.delay", ["ms": 300, "value": "slow"])
        async let quick = c.request("test.delay", ["ms": 0, "value": "quick"])
        try await Task.sleep(for: .milliseconds(100))
        fake.notify("exec.output", ["pid": 7, "stream": "stdout", "data": .string(Data("hi".utf8).base64EncodedString())])
        fake.notify("exec.exit", ["pid": 7, "code": 3])
        #expect(try await quick["value"] == "quick")
        #expect(try await slow["value"] == "slow")
        var seen: [GuestEvent] = []
        for await e in events {
            seen.append(e)
            if seen.count == 2 { break }
        }
        #expect(seen == [.output(pid: 7, stream: "stdout", data: Data("hi".utf8)), .exit(pid: 7, code: 3)])
    }

    @Test(arguments: ["file.write", "sync.apply"])
    func slashHeavyBinaryChunksReachTheGuestIntact(method: String) async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        defer { fake.stop() }
        let c = channel(fake.path)
        await c.start()
        defer { Task { await c.stop() } }
        await eventually { await c.isReady }
        // A full binary chunk whose base64 consists almost entirely of slashes.
        let data = Data(repeating: 0xff, count: 512 * 1024)
        let path = "C:\\transfer\\payload.bin"
        let params: JSONValue
        if method == "file.write" {
            params = ["path": .string(path), "offset": 0, "data": .string(data.base64EncodedString()), "truncate": true, "mkdirs": true]
        } else {
            let op: JSONValue = [
                "op": "write", "path": "payload.bin", "offset": 0, "data": .string(data.base64EncodedString()),
                "end": true, "hash": .string(FakeGuestService.sha(data)),
            ]
            params = ["id": "transfer", "root": "C:\\transfer", "ops": .array([op]), "commit": true]
        }
        _ = try await c.request(method, params)
        #expect(fake.files[path] == data)
    }

    @Test func remoteErrorsTimeoutsAndOversizedRequestsAreReported() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        defer { fake.stop() }
        let c = channel(fake.path)
        await c.start()
        defer { Task { await c.stop() } }
        await eventually { await c.isReady }
        await #expect(throws: GuestServiceError.remote(code: 3, message: "denied")) { try await c.request("test.fail") }
        await #expect(throws: GuestServiceError.timeout("test.silent")) { try await c.request("test.silent", timeout: .milliseconds(100)) }
        let big = JSONValue.string(String(repeating: "x", count: 2 << 20))
        await #expect(throws: GuestServiceError.self) { try await c.request("test.delay", ["value": big]) }
        // Still usable afterwards.
        #expect(try await c.request("test.delay", ["ms": 0, "value": 1])["value"] == 1)
    }

    @Test func aFrameOverOneMiBDropsTheConnectionAndTheHostReconnects() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        defer { fake.stop() }
        let c = channel(fake.path)
        await c.start()
        defer { Task { await c.stop() } }
        await eventually { await c.isReady }
        let before = fake.accepted
        fake.rawWrite(Data([0x00, 0x10, 0x00, 0x01]))  // claims 1 MiB + 1 bytes
        await eventually { fake.accepted > before }
        await eventually { await c.isReady }
    }

    @Test(.timeLimit(.minutes(1))) func aReconnectIsANewSession() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        defer { fake.stop() }
        let c = channel(fake.path)
        await c.start()
        defer { Task { await c.stop() } }
        await eventually { await c.isReady }
        let events = await c.events()
        fake.bootId = "boot-2"
        fake.dropConnection()
        var session: GuestInfo?
        // A probe answered on the old connection may still announce boot-1 first.
        for await e in events { if case .session(let i) = e, i.bootId == "boot-2" { session = i; break } }
        #expect(session?.bootId == "boot-2")
        #expect(fake.requests("guest.info").count >= 2)
    }

    @Test func aNewerProtocolIsRefused() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        fake.protocolVersion = 2
        defer { fake.stop() }
        let c = channel(fake.path)
        await c.start()
        defer { Task { await c.stop() } }
        await eventually { await c.problem != nil }
        #expect(await c.isReady == false)
    }
}

/// A transport like VZ's vsock: connecting fails until the Guest Service listens.
private final class RefusingConnector: @unchecked Sendable {
    private let lock = NSLock()
    private var _attempts = 0
    private var _listening = false
    let path: String
    init(path: String) { self.path = path }
    var attempts: Int { lock.withLock { _attempts } }
    var listening: Bool { get { lock.withLock { _listening } } set { lock.withLock { _listening = newValue } } }
    func connect() async throws -> Int32 {
        let up = lock.withLock { _attempts += 1; return _listening }
        guard up else { throw GuestServiceError.notConnected }
        return try await Blocking.run { [path] in try UnixSocket.connect(path: path) }
    }
}

@Suite struct GuestChannelConnectorTests {
    @Test func aRefusedConnectBacksOffAndTheChannelConnectsOnceTheGuestServiceListens() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fake = try FakeGuestService(socketPath: dir + "/guest.sock")
        defer { fake.stop() }
        let connector = RefusingConnector(path: fake.path)
        let c = GuestChannel(retryInterval: .milliseconds(20), maxRetryInterval: .milliseconds(160), probeInterval: .milliseconds(100)) {
            try await connector.connect()
        }
        await c.start()
        defer { Task { await c.stop() } }
        // A loaded host only slows the retries, so wait for the first few rather than counting them after a fixed sleep.
        await eventually("the channel never retried") { connector.attempts >= 3 }
        try await Task.sleep(for: .milliseconds(700))
        // Without backoff that would be about 45 attempts; doubling to 160 ms gives under 10.
        #expect(connector.attempts <= 10)
        #expect(await c.isReady == false)
        connector.listening = true
        await eventually { await c.isReady }
        #expect(await c.info?.bootId == "boot-1")

        // A dropped connection reconnects at the base interval, as a new session.
        let events = await c.events()
        fake.bootId = "boot-2"
        fake.dropConnection()
        var session: GuestInfo?
        // A probe answered on the old connection may still announce boot-1 first.
        for await e in events { if case .session(let i) = e, i.bootId == "boot-2" { session = i; break } }
        #expect(session?.bootId == "boot-2")
    }
}
