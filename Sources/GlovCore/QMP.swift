import Foundation

public struct QMPEvent: Sendable, Equatable {
    public var name: String
    public var data: JSONValue?
}

public enum QMPError: Error, Equatable, CustomStringConvertible {
    case connectFailed(path: String, errno: Int32)
    case closed
    case command(class: String, desc: String)

    public var description: String {
        switch self {
        case .connectFailed(let p, let e): "cannot connect to \(p): \(String(cString: strerror(e)))"
        case .closed: "QMP connection closed"
        case .command(let c, let d): "QMP \(c): \(d)"
        }
    }
}

private struct QMPMessage: Decodable {
    var id: Int?
    var result: JSONValue?
    var error: ErrorBody?
    var event: String?
    var data: JSONValue?
    var greeting: JSONValue?

    struct ErrorBody: Decodable { var `class`: String; var desc: String }
    enum CodingKeys: String, CodingKey { case id, result = "return", error, event, data, greeting = "QMP" }
}

/// Async QMP client over a unix domain socket. One reader thread feeds an actor that
/// correlates replies by `id` and fans events out to subscribers.
public actor QMPClient {
    private let handle: UnixSocket.Handle
    private var nextID = 1
    private var pending: [Int: CheckedContinuation<JSONValue, any Error>] = [:]
    private var subscribers: [UUID: AsyncStream<QMPEvent>.Continuation] = [:]
    private var closed = false
    private var greeted = false
    private var greetingWaiter: CheckedContinuation<Void, any Error>?

    private init(handle: UnixSocket.Handle) { self.handle = handle }

    /// Connects, reads the greeting and negotiates capabilities.
    public static func connect(socketPath: String) async throws -> QMPClient {
        let handle = UnixSocket.Handle(fd: try await Blocking.run { try UnixSocket.connect(path: socketPath) })
        let client = QMPClient(handle: handle)
        let lines = UnixSocket.lines(fd: handle.fd, onEnd: { handle.close() })
        Task { await client.run(lines) }
        try await client.waitForGreeting()
        _ = try await client.execute("qmp_capabilities")
        return client
    }

    public func execute(_ command: String, _ arguments: JSONValue? = nil) async throws -> JSONValue {
        guard !closed else { throw QMPError.closed }
        let id = nextID
        nextID += 1
        var msg: [String: JSONValue] = ["execute": .string(command), "id": .int(id)]
        if let arguments { msg["arguments"] = arguments }
        let line = try JSONEncoder().encode(JSONValue.object(msg)) + Data("\n".utf8)
        return try await withCheckedThrowingContinuation { c in
            pending[id] = c
            // Not awaited, so the actor keeps handling replies while a write waits; replies are matched by id.
            Task { if await !handle.send(line) { fail(id) } }
        }
    }

    private func fail(_ id: Int) { pending.removeValue(forKey: id)?.resume(throwing: QMPError.closed) }

    /// Events received after this call. The stream ends when the connection closes.
    public func events() -> AsyncStream<QMPEvent> {
        let (stream, cont) = AsyncStream<QMPEvent>.makeStream()
        if closed { cont.finish(); return stream }
        let key = UUID()
        subscribers[key] = cont
        cont.onTermination = { [weak self] _ in Task { await self?.unsubscribe(key) } }
        return stream
    }

    /// Ends the reader, which then closes the fd (see `UnixSocket.Handle`).
    public func close() { handle.shutdown() }

    private func unsubscribe(_ key: UUID) { subscribers[key] = nil }

    private func waitForGreeting() async throws {
        if greeted { return }
        if closed { throw QMPError.closed }
        try await withCheckedThrowingContinuation { greetingWaiter = $0 }
    }

    private func run(_ lines: AsyncStream<Data>) async {
        for await line in lines { handle(line) }
        closed = true
        greetingWaiter?.resume(throwing: QMPError.closed)
        greetingWaiter = nil
        for c in pending.values { c.resume(throwing: QMPError.closed) }
        pending = [:]
        for s in subscribers.values { s.finish() }
        subscribers = [:]
    }

    private func handle(_ line: Data) {
        guard let m = try? JSONDecoder().decode(QMPMessage.self, from: line) else { return }
        if m.greeting != nil {
            greeted = true
            greetingWaiter?.resume()
            greetingWaiter = nil
        } else if let name = m.event {
            for s in subscribers.values { s.yield(QMPEvent(name: name, data: m.data)) }
        } else if let id = m.id, let c = pending.removeValue(forKey: id) {
            if let e = m.error { c.resume(throwing: QMPError.command(class: e.class, desc: e.desc)) }
            else { c.resume(returning: m.result ?? .null) }
        }
    }
}

/// POSIX unix-socket plumbing shared by the QMP client, the daemon and its clients.
public enum UnixSocket {
    public static func address(_ path: String) -> sockaddr_un? {
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        guard bytes.count < MemoryLayout.size(ofValue: addr.sun_path) else { return nil }
        withUnsafeMutableBytes(of: &addr.sun_path) { $0.copyBytes(from: bytes + [0]) }
        return addr
    }

    /// Blocks while the listener's backlog is full (a hung QEMU, a busy daemon), so async code goes through `Blocking.run`.
    @available(*, noasync, message: "can block a cooperative thread; call it through Blocking.run")
    public static func connect(path: String) throws -> Int32 {
        guard var addr = address(path) else { throw GlovError.socketPathTooLong(path) }
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw QMPError.connectFailed(path: path, errno: errno) }
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Foundation.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard rc == 0 else {
            let e = errno
            Foundation.close(fd)
            throw QMPError.connectFailed(path: path, errno: e)
        }
        return fd
    }

    /// Binds and listens on `path`, then sets mode 0600 (replacing nothing: the caller removes a stale socket first).
    public static func listen(path: String) throws -> Int32 {
        guard var addr = address(path) else { throw GlovError.socketPathTooLong(path) }
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw QMPError.connectFailed(path: path, errno: errno) }
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        // chmod, not a temporary umask: umask is process-wide and would race with other threads creating files.
        // The window is harmless because the home directory is 0700.
        guard rc == 0, chmod(path, 0o600) == 0, Foundation.listen(fd, 16) == 0 else {
            let e = errno
            Foundation.close(fd)
            throw QMPError.connectFailed(path: path, errno: e)
        }
        return fd
    }

    /// A listening socket whose accept thread is the only code that ever closes it. Closing a listener from another
    /// thread races that thread's next `accept`: the number may already belong to another socket, whose clients the
    /// stale thread would then take. So `stop()` only flags, and the thread notices within `pollInterval`.
    public final class Listener: @unchecked Sendable {
        public let fd: Int32
        private let lock = NSLock()
        private var stopped = false
        private var started = false
        private let pollInterval: Int32 = 50  // ms
        private let closeFD: @Sendable (Int32) -> Void

        /// `closeFD` lets a test observe the close; a connect to the path cannot, since a child spawned meanwhile
        /// holds a copy of the socket until its exec.
        public init(fd: Int32, closeFD: @escaping @Sendable (Int32) -> Void = { _ = Foundation.close($0) }) {
            self.fd = fd
            self.closeFD = closeFD
        }

        /// Accepts on a background thread until `stop()`, handing each new connection to `handler`.
        public func start(_ handler: @escaping @Sendable (Int32) -> Void) {
            lock.withLock { started = true }
            Thread.detachNewThread { [self] in
                while !lock.withLock({ stopped }) {
                    var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
                    let r = poll(&p, 1, pollInterval)
                    if r <= 0 { if r < 0 && errno != EINTR { break }; continue }
                    let client = accept(fd, nil, nil)
                    if client < 0 {
                        if errno == EINTR || errno == EAGAIN || errno == ECONNABORTED { continue }
                        break
                    }
                    if lock.withLock({ stopped }) { Foundation.close(client); break }
                    handler(client)
                }
                closeFD(fd)
            }
        }

        /// Stops accepting; the descriptor is closed by the accept thread (or here, if it never started).
        public func stop() {
            let closeHere = lock.withLock { () -> Bool in
                defer { stopped = true }
                return !started && !stopped
            }
            if closeHere { closeFD(fd) }
        }
    }

    /// A connected fd shared by a reader thread and writers on other threads. Only the reader closes it, after its
    /// last `read` (pass `close` as the reader's `onEnd`); everyone else calls `shutdown` to end the reader. Once
    /// closed the number may belong to another socket, so `shutdown` and `write` are no-ops from then on.
    public final class Handle: @unchecked Sendable {
        public let fd: Int32
        private let lock = NSLock()
        private var closed = false
        private let writer = DispatchQueue(label: "org.glov.socket-writer")
        public init(fd: Int32) { self.fd = fd }
        @available(*, noasync, message: "use send(_:), which blocks a writer queue instead of a cooperative thread")
        public func write(_ data: Data) -> Bool { lock.withLock { !closed && UnixSocket.write(fd: fd, data) } }
        /// `write` from async code, on this handle's serial queue: the writes of one actor go out in call order.
        public nonisolated(nonsending) func send(_ data: Data) async -> Bool { await Blocking.run(on: writer) { self.write(data) } }
        public func shutdown() { lock.withLock { if !closed { Darwin.shutdown(fd, SHUT_RDWR) } } }
        public func close() { lock.withLock { if !closed { closed = true; Foundation.close(fd) } } }
    }

    /// `read(2)` that first waits in `poll` until `fd` is readable. On macOS (seen on 27.2) a thread blocked in `read` on
    /// a unix socket can sleep for good after the peer's `shutdown` from another thread, about once in ten thousand: the
    /// socket is at EOF but the wakeup is lost. `poll` and kqueue do not miss it. Use this wherever a peer may end a
    /// stream with `shutdown`.
    @available(*, noasync, message: "blocks on the socket; run it on a thread of its own")
    public static func readWhenReadable(_ fd: Int32, _ buffer: UnsafeMutableRawBufferPointer) -> Int {
        var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
        while poll(&p, 1, -1) < 0 && errno == EINTR {}
        return Foundation.read(fd, buffer.baseAddress, buffer.count)
    }

    /// Blocks until all of `data` is written; async code goes through `Handle.send` or `Blocking.run`.
    @available(*, noasync, message: "blocks a cooperative thread; use Handle.send or Blocking.run")
    public static func write(fd: Int32, _ data: Data) -> Bool {
        var off = 0
        return data.withUnsafeBytes { buf in
            while off < buf.count {
                let n = Foundation.write(fd, buf.baseAddress! + off, buf.count - off)
                if n < 0 && errno == EINTR { continue }
                if n <= 0 { return false }
                off += n
            }
            return true
        }
    }

    /// Newline-delimited messages from `fd`; ends on EOF or error. Does not close `fd`.
    /// `onEnd` runs on the reader thread after its last `read`: the safe place to close the fd (see `Handle`).
    public static func lines(fd: Int32, onEnd: @escaping @Sendable () -> Void = {}) -> AsyncStream<Data> {
        AsyncStream { cont in
            Thread.detachNewThread {
                defer { onEnd() }
                var buf = [UInt8](repeating: 0, count: 65536)
                var acc: [UInt8] = []
                while true {
                    let n = read(fd, &buf, buf.count)
                    if n < 0 && errno == EINTR { continue }
                    if n <= 0 { break }
                    // Only the new bytes are searched: a screenshot reply is one line of about 1.4 MB, and rescanning
                    // the whole buffer per read made receiving it take hundreds of milliseconds.
                    var from = acc.count
                    acc.append(contentsOf: buf[..<n])
                    var lineStart = 0
                    while let nl = acc.withUnsafeBufferPointer({ p -> Int? in
                        memchr(p.baseAddress! + from, 10, p.count - from).map { p.baseAddress!.distance(to: $0.assumingMemoryBound(to: UInt8.self)) }
                    }) {
                        cont.yield(Data(acc[lineStart..<nl]))
                        lineStart = nl + 1
                        from = lineStart
                    }
                    if lineStart > 0 { acc.removeFirst(lineStart) }
                }
                cont.finish()
            }
        }
    }
}
