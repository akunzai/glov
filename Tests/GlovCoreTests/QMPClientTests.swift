import Foundation
import Testing

@testable import GlovCore

@Suite struct QMPClientTests {
    @Test func negotiatesCapabilitiesAndReturnsResult() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let server = try FakeQMPServer(dir: dir)
        let client = try await QMPClient.connect(socketPath: server.path)
        let reply = try await client.execute("echo")
        #expect(reply == ["cmd": "echo"])
        #expect(server.received == ["qmp_capabilities", "echo"])
    }

    @Test func concurrentCommandsGetTheirOwnReplies() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let client = try await QMPClient.connect(socketPath: try FakeQMPServer(dir: dir).path)
        let replies = try await withThrowingTaskGroup(of: String.self) { g in
            for n in 0..<20 { g.addTask { "c\(n)" == (try await client.execute("c\(n)"))["cmd"]?.stringValue ? "ok" : "mismatch" } }
            return try await g.reduce(into: []) { $0.append($1) }
        }
        #expect(replies == Array(repeating: "ok", count: 20))
    }

    @Test func surfacesCommandErrors() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let client = try await QMPClient.connect(socketPath: try FakeQMPServer(dir: dir).path)
        await #expect(throws: QMPError.command(class: "GenericError", desc: "boom")) { try await client.execute("fail") }
    }

    @Test func deliversEventsToSubscribers() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let client = try await QMPClient.connect(socketPath: try FakeQMPServer(dir: dir).path)
        let events = await client.events()
        _ = try await client.execute("shutdown")
        var it = events.makeAsyncIterator()
        let event = await it.next()
        #expect(event?.name == "SHUTDOWN")
        #expect(event?.data?["reason"] == "guest-shutdown")
    }

    @Test func failsPendingCommandsAndEndsEventsWhenServerHangsUp() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let client = try await QMPClient.connect(socketPath: try FakeQMPServer(dir: dir).path)
        let events = await client.events()
        await #expect(throws: QMPError.closed) { try await client.execute("hangup") }
        var it = events.makeAsyncIterator()
        #expect(await it.next() == nil)
        await #expect(throws: QMPError.closed) { try await client.execute("echo") }
    }

    @Test func reportsMissingSocket() async {
        await #expect(throws: QMPError.self) { try await QMPClient.connect(socketPath: "/tmp/gp.does-not-exist/qmp.sock") }
    }
}

extension JSONValue {
    var stringValue: String? { if case .string(let s) = self { s } else { nil } }
}

@Suite struct ListenerTests {
    @Test func acceptsUntilStoppedAndThenRefuses() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let path = dir + "/l.sock"
        let accepted = Counter()
        let l = UnixSocket.Listener(fd: try UnixSocket.listen(path: path))
        l.start { fd in accepted.add(); close(fd) }
        close(try await Blocking.run { try UnixSocket.connect(path: path) })
        let deadline = ContinuousClock.now + .seconds(2)
        while accepted.value == 0 && ContinuousClock.now < deadline { try await Task.sleep(for: .milliseconds(10)) }
        #expect(accepted.value == 1)
        l.stop()
        try await Task.sleep(for: .milliseconds(200))  // the accept thread notices within its 50 ms poll and closes the socket
        await #expect(throws: QMPError.self) { close(try await Blocking.run { try UnixSocket.connect(path: path) }) }
        #expect(accepted.value == 1)
    }

    @Test func stoppingAListenerThatNeverStartedClosesIt() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fd = try UnixSocket.listen(path: dir + "/l.sock")
        let closed = Recorder()
        let l = UnixSocket.Listener(fd: fd, closeFD: { closed.add($0); close($0) })
        l.stop()
        #expect(closed.values == [fd])
        l.stop()
        #expect(closed.values == [fd])
    }
}

private final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0
    var value: Int { lock.withLock { n } }
    func add() { lock.withLock { n += 1 } }
}

@Suite struct LinesTests {
    @Test func splitsALongLineThatArrivesInManyReadsAndTheLinesAroundIt() async throws {
        var fds: [Int32] = [0, 0]
        #expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &fds) == 0)
        let big = Data(repeating: UInt8(ascii: "x"), count: 1_500_000)
        let writer = fds[1]
        Thread.detachNewThread {
            _ = UnixSocket.write(fd: writer, Data("a\n".utf8) + big + Data("\nb\nc".utf8))
            close(writer)
        }
        var got: [Data] = []
        for await line in UnixSocket.lines(fd: fds[0], onEnd: { [fd = fds[0]] in close(fd) }) { got.append(line) }
        #expect(got.count == 3)  // "c" has no newline, so it is not a line
        #expect(got.first == Data("a".utf8) && got.last == Data("b".utf8))
        #expect(got.count == 3 && got[1] == big)
    }
}

/// More blocked callers than the cooperative pool has threads: if any of them held a pool thread, the work after them
/// could never run and the test would hang.
@Suite struct BlockingTests {
    private let callers = ProcessInfo.processInfo.activeProcessorCount + 2

    /// Needs a pool thread for every child task.
    private func poolStillRuns() async -> Bool {
        let n = callers
        return await withTaskGroup(of: Int.self) { g in
            for i in 0..<n { g.addTask { i } }
            return await g.reduce(0, +) == n * (n - 1) / 2
        }
    }

    @Test(.timeLimit(.minutes(1))) func blockingWorkWaitsOnThreadsOfItsOwn() async throws {
        let gate = DispatchSemaphore(value: 0)
        let waiters = (0..<callers).map { _ in Task { await Blocking.run { gate.wait() } } }
        try await Task.sleep(for: .milliseconds(100))
        #expect(await poolStillRuns())
        for _ in waiters { gate.signal() }
        for w in waiters { await w.value }
    }

    @Test(.timeLimit(.minutes(1))) func aSendToAPeerThatDoesNotReadLeavesThePoolFree() async throws {
        var pairs: [(handle: UnixSocket.Handle, peer: Int32)] = []
        for _ in 0..<callers {
            var fds: [Int32] = [0, 0]
            #expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &fds) == 0)
            var one: Int32 = 1
            setsockopt(fds[0], SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
            pairs.append((UnixSocket.Handle(fd: fds[0]), fds[1]))
        }
        let big = Data(count: 4 << 20)  // far more than a socket buffer holds, so each send blocks until its peer goes
        let sends = pairs.map { pair in Task { await pair.handle.send(big) } }
        try await Task.sleep(for: .milliseconds(100))
        #expect(await poolStillRuns())
        for pair in pairs { close(pair.peer) }
        for s in sends { #expect(await s.value == false) }
        for pair in pairs { pair.handle.close() }
    }
}

private final class Recorder: @unchecked Sendable {
    private let lock = NSLock()
    private var all: [Int32] = []
    var values: [Int32] { lock.withLock { all } }
    func add(_ v: Int32) { lock.withLock { all.append(v) } }
}
