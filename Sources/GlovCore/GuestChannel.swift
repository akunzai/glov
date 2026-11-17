import Foundation

/// What `guest.info` and `guest.ready` report (docs/guest-protocol.md).
public struct GuestInfo: Codable, Sendable, Equatable {
    public var protocolVersion: Int
    public var version: String
    public var os: String
    public var arch: String
    public var hostname: String
    public var user: String?
    public var bootId: String

    public init(
        protocolVersion: Int = GuestChannel.protocolVersion, version: String = "", os: String = "windows", arch: String = "arm64",
        hostname: String = "", user: String? = nil, bootId: String = ""
    ) {
        self.protocolVersion = protocolVersion; self.version = version; self.os = os; self.arch = arch
        self.hostname = hostname; self.user = user; self.bootId = bootId
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        protocolVersion = try c.decode(Int.self, forKey: .protocolVersion)
        version = try c.decodeIfPresent(String.self, forKey: .version) ?? ""
        os = try c.decode(String.self, forKey: .os)
        arch = try c.decodeIfPresent(String.self, forKey: .arch) ?? ""
        hostname = try c.decodeIfPresent(String.self, forKey: .hostname) ?? ""
        user = try c.decodeIfPresent(String.self, forKey: .user)
        // The protocol does not fix the type of bootId; accept a number as well as a string.
        if let s = try? c.decode(String.self, forKey: .bootId) { bootId = s }
        else if let n = try? c.decode(Int.self, forKey: .bootId) { bootId = String(n) }
        else { bootId = "" }
    }
}

/// What the Guest Service pushes: a new session (after `guest.info` succeeded), output chunks and process exits.
public enum GuestEvent: Sendable, Equatable {
    case session(GuestInfo)
    case output(pid: Int, stream: String, data: Data)
    case exit(pid: Int, code: Int)
}

public enum GuestServiceError: Error, Equatable, CustomStringConvertible {
    /// The Guest Service's "no signed-in user" error (`CodeNoUser`, docs/guest-protocol.md).
    public static let noUserCode = 4
    case notConnected
    case notResponding(String)
    case timeout(String)
    case remote(code: Int, message: String)
    case protocolViolation(String)

    public var description: String {
        switch self {
        case .notConnected: "the Guest Service is not connected (is the VM running and the Guest Service installed? see vm_wait guest-ready)"
        case .notResponding(let m): "Guest Service not responding: \(m) (the guest may be rebooting or hung; see vm_status)"
        case .timeout(let m): "the Guest Service did not answer \(m) in time"
        case .remote(let c, let m): "Guest Service error \(c): \(m)"
        case .protocolViolation(let m): "Guest Service protocol error: \(m)"
        }
    }
}

/// 4-byte big-endian length + JSON frames.
enum GuestFraming {
    static let maxFrame = 1 << 20

    static func encode(_ payload: Data) -> Data {
        var n = UInt32(payload.count).bigEndian
        return Data(bytes: &n, count: 4) + payload
    }

    /// Frames from `fd`; ends on EOF, error, or a frame over 1 MiB (the fd is shut down then: the sender is broken).
    /// `onEnd` runs on the reader thread after its last `read`, so it is the one safe place to close the fd:
    /// a consumer that stops iterating (for example when its task is cancelled) does not stop the thread.
    static func frames(fd: Int32, onEnd: @escaping @Sendable () -> Void = {}) -> AsyncStream<Data> {
        AsyncStream { cont in
            Thread.detachNewThread {
                defer { onEnd() }
                func readExactly(_ n: Int) -> [UInt8]? {
                    var out = [UInt8](repeating: 0, count: n)
                    var got = 0
                    while got < n {
                        let r = out.withUnsafeMutableBytes { read(fd, $0.baseAddress! + got, n - got) }
                        if r < 0 && errno == EINTR { continue }
                        if r <= 0 { return nil }
                        got += r
                    }
                    return out
                }
                while let header = readExactly(4) {
                    let length = header.reduce(0) { Int($0) << 8 | Int($1) }
                    if length > maxFrame { shutdown(fd, SHUT_RDWR); break }
                    guard let body = readExactly(length) else { break }
                    cont.yield(Data(body))
                }
                cont.finish()
            }
        }
    }
}

/// The host end of the Guest Service channel: connects through its `Connector` (the QEMU chardev socket `guest.sock`,
/// or a VZ virtio-vsock port) and keeps reconnecting for as long as it runs, because the Guest Service may come up minutes after boot.
///
/// Connecting alone proves nothing (QEMU accepts before the guest opens the port), so `info` is the signal:
/// it is set once `guest.info` has been answered, after an `guest.ready` or one of a few probes sent right after connecting.
/// A session is kept alive by a heartbeat (`guest.info` every `heartbeatInterval`), because virtio-serial gives no EOF when
/// the guest goes away: `heartbeatMisses` unanswered ones in a row, with no other frame in between, make it `unresponsive`.
public actor GuestChannel {
    public static let protocolVersion = 1

    /// Opens a new connected stream to the Guest Service and returns its file descriptor, which the channel then owns.
    public typealias Connector = @Sendable () async throws -> Int32
    public typealias Log = @Sendable (String) -> Void

    private let connector: Connector
    /// Where session changes and dropped frames are reported: the daemon's log, prefixed with the VM name.
    private let log: Log
    /// Non-nil while a session is established, including while it is unresponsive (then `user` and `bootId` may be stale).
    public private(set) var info: GuestInfo?
    /// The session's agent stopped answering; it continues if the Guest Service answers again from the same boot.
    public private(set) var unresponsive = false
    /// Why the Guest Service is connected but unusable (for example a newer protocol, or no reply), if so.
    public var problem: String? {
        if unresponsive { return "no reply from the Guest Service for \((ContinuousClock.now - lastFrame).components.seconds) s" }
        return protocolProblem
    }
    public var isReady: Bool { info != nil && !unresponsive }
    private var protocolProblem: String?

    private var task: Task<Void, Never>?
    private var writer: UnixSocket.Handle?
    private var nextID = 1
    private var pending: [Int: CheckedContinuation<JSONValue, any Error>] = [:]
    /// The method of each request still waiting, but heartbeats, so that an answer arriving after its request gave up
    /// can be logged by name; a late heartbeat answer is routine under load and is not.
    private var methods: [Int: String] = [:]
    private var subscribers: [UUID: AsyncStream<GuestEvent>.Continuation] = [:]
    private var stopped = false
    private let retryInterval: Duration
    private let maxRetryInterval: Duration
    private let probeInterval: Duration
    private let heartbeatInterval: Duration
    private let heartbeatTimeout: Duration
    private let heartbeatMisses: Int
    private var misses = 0
    /// Frames received so far, and when the last one came: any frame counts as a heartbeat reply.
    private var received = 0
    private var lastFrame = ContinuousClock.now

    /// The QEMU virtio-serial chardev socket: it accepts as soon as QEMU runs, so retries stay at `retryInterval`.
    public init(
        socketPath: String, retryInterval: Duration = .milliseconds(500), probeInterval: Duration = .seconds(5),
        heartbeatInterval: Duration = .seconds(5), heartbeatTimeout: Duration = .seconds(3), heartbeatMisses: Int = 2,
        log: @escaping Log = { _ in }
    ) {
        self.init(
            retryInterval: retryInterval, probeInterval: probeInterval, heartbeatInterval: heartbeatInterval, heartbeatTimeout: heartbeatTimeout,
            heartbeatMisses: heartbeatMisses, log: log
        ) { try await Blocking.run { try UnixSocket.connect(path: socketPath) } }
    }

    /// Any stream transport. A failed connect is retried after `retryInterval`, doubling up to `maxRetryInterval`
    /// (a vsock connect fails until the Guest Service listens); a connection that worked resets the delay.
    public init(
        retryInterval: Duration = .milliseconds(500), maxRetryInterval: Duration? = nil, probeInterval: Duration = .seconds(5),
        heartbeatInterval: Duration = .seconds(5), heartbeatTimeout: Duration = .seconds(3), heartbeatMisses: Int = 2,
        log: @escaping Log = { _ in }, connector: @escaping Connector
    ) {
        self.connector = connector
        self.log = log
        self.retryInterval = retryInterval
        self.maxRetryInterval = max(maxRetryInterval ?? retryInterval, retryInterval)
        self.probeInterval = probeInterval
        self.heartbeatInterval = heartbeatInterval
        self.heartbeatTimeout = heartbeatTimeout
        self.heartbeatMisses = heartbeatMisses
    }

    public func start() {
        guard task == nil, !stopped else { return }
        task = Task { await self.run() }
    }

    /// Closes the connection and stops reconnecting. The channel cannot be restarted.
    public func stop() {
        stopped = true
        task?.cancel()
        writer?.shutdown()
    }

    /// Drops the current session and connects again, as after the guest was restored from a snapshot:
    /// the old session's state belongs to the past. `info` is nil until the Guest Service answers on the new connection.
    public func reset() {
        endSession()
        writer?.shutdown()
    }

    /// Events received after this call. The stream ends when the channel stops.
    public func events() -> AsyncStream<GuestEvent> {
        let (stream, cont) = AsyncStream<GuestEvent>.makeStream(bufferingPolicy: .unbounded)
        if stopped { cont.finish(); return stream }
        let key = UUID()
        subscribers[key] = cont
        cont.onTermination = { [weak self] _ in Task { await self?.unsubscribe(key) } }
        return stream
    }

    /// A request to the established session; throws `notConnected` before the first `guest.info` answer
    /// and `notResponding` while the session is unresponsive. With `cancellable`, cancelling the calling task stops
    /// waiting for the answer and throws `CancellationError` (the guest is not told; the caller does that). Otherwise
    /// the request waits for its answer or its timeout, as a request whose answer must not be lost (`exec.start`) needs.
    public func request(
        _ method: String, _ params: JSONValue? = nil, timeout: Duration = .seconds(30), cancellable: Bool = false
    ) async throws -> JSONValue {
        guard info != nil else { throw GuestServiceError.notConnected }
        if unresponsive { throw GuestServiceError.notResponding(problem ?? "no reply") }
        guard cancellable else { return try await send(method, params, timeout: timeout) }
        let id = nextID
        nextID += 1
        try Task.checkCancellation()
        return try await withTaskCancellationHandler {
            try await send(id, method, params, timeout: timeout, cancellable: true)
        } onCancel: {
            Task { await self.cancelWait(id) }
        }
    }

    private func unsubscribe(_ key: UUID) { subscribers[key] = nil }

    private func emit(_ event: GuestEvent) { for s in subscribers.values { s.yield(event) } }

    private func send(_ method: String, _ params: JSONValue?, timeout: Duration) async throws -> JSONValue {
        let id = nextID
        nextID += 1
        return try await send(id, method, params, timeout: timeout, cancellable: false)
    }

    private func send(_ id: Int, _ method: String, _ params: JSONValue?, timeout: Duration, cancellable: Bool) async throws -> JSONValue {
        guard let writer else { throw GuestServiceError.notConnected }
        var message: [String: JSONValue] = ["jsonrpc": "2.0", "id": .int(id), "method": .string(method)]
        if let params { message["params"] = params }
        let encoder = JSONEncoder()
        // Escaping base64 slashes can double a binary chunk beyond the frame limit.
        encoder.outputFormatting = .withoutEscapingSlashes
        let frame = GuestFraming.encode(try encoder.encode(JSONValue.object(message)))
        guard frame.count - 4 <= GuestFraming.maxFrame else { throw GuestServiceError.protocolViolation("request larger than 1 MiB") }
        let timer = Task {
            try? await Task.sleep(for: timeout)
            self.fail(id, GuestServiceError.timeout("\(method)"))
        }
        defer { timer.cancel() }
        return try await withCheckedThrowingContinuation { c in
            pending[id] = c
            if method != "guest.info" { methods[id] = method }
            // Cancelled before the wait began: the handler's `cancelWait` may have run already and found nothing.
            if cancellable && Task.isCancelled {
                cancelWait(id)
                return
            }
            // Not awaited on the actor: a full socket buffer must not stall response handling. The fd has a send timeout.
            Task { if await !writer.send(frame) { fail(id, GuestServiceError.notConnected) } }
        }
    }

    private func fail(_ id: Int, _ error: GuestServiceError) { pending.removeValue(forKey: id)?.resume(throwing: error) }

    /// Stops waiting for `id`'s answer; one that arrives later is dropped as unknown.
    private func cancelWait(_ id: Int) { pending.removeValue(forKey: id)?.resume(throwing: CancellationError()) }

    private func run() async {
        var delay = retryInterval
        while !stopped {
            if let fd = try? await connector() {
                var timeout = timeval(tv_sec: 10, tv_usec: 0)
                setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
                await session(fd)
                delay = retryInterval
                if !stopped { try? await Task.sleep(for: delay) }
            } else {
                if !stopped { try? await Task.sleep(for: delay) }
                delay = min(delay * 2, maxRetryInterval)
            }
        }
        for s in subscribers.values { s.finish() }
        subscribers = [:]
    }

    private func session(_ fd: Int32) async {
        let owner = UnixSocket.Handle(fd: fd)
        writer = owner
        let probing = Task {
            for _ in 0..<6 where !Task.isCancelled {
                if isReady { return }
                try? await establish()
                try? await Task.sleep(for: self.probeInterval)
            }
        }
        // On a fixed cadence, so a hang is noticed within heartbeatMisses * heartbeatInterval + heartbeatTimeout.
        let heartbeat = Task {
            while !Task.isCancelled {
                let start = ContinuousClock.now
                if info != nil { await beat() }
                try? await Task.sleep(until: start + self.heartbeatInterval, clock: .continuous)
            }
        }
        lastFrame = .now
        for await frame in GuestFraming.frames(fd: fd, onEnd: { owner.close() }) { handle(frame) }
        probing.cancel()
        heartbeat.cancel()
        writer?.shutdown()
        writer = nil
        if info != nil { log("Guest Service connection closed") }
        endSession()
        methods = [:]
        let waiting = pending
        pending = [:]
        for c in waiting.values { c.resume(throwing: GuestServiceError.notConnected) }
    }

    private func endSession() {
        info = nil
        protocolProblem = nil
        unresponsive = false
        misses = 0
    }

    /// Asks `guest.info` and adopts the answer: as a new session unless it continues the current one's boot.
    private func establish() async throws {
        let result = try await send("guest.info", nil, timeout: heartbeatTimeout)
        let adopted = try JSONDecoder().decode(GuestInfo.self, from: JSONEncoder().encode(result))
        guard adopted.protocolVersion <= Self.protocolVersion else {
            protocolProblem = "the Guest Service speaks protocol \(adopted.protocolVersion), this host knows \(Self.protocolVersion): update glov"
            return
        }
        protocolProblem = adopted.protocolVersion < Self.protocolVersion ? "the Guest Service is older than this host: update the Guest Service" : nil
        let isNew = info?.bootId != adopted.bootId
        info = adopted
        if unresponsive {
            unresponsive = false
            log("Guest Service answers again")
        }
        if isNew {
            log("Guest Service session: \(adopted.os) \(adopted.version), boot \(adopted.bootId)")
            emit(.session(adopted))
        }
    }

    /// One heartbeat: refreshes `info` (the signed-in user changes), or counts a miss if nothing at all came back in time.
    private func beat() async {
        let before = received
        do { try await establish() } catch GuestServiceError.timeout {
            guard received == before, info != nil else { return }
            misses += 1
            if misses >= heartbeatMisses && !unresponsive {
                unresponsive = true
                log("Guest Service stopped answering: \(misses) heartbeats in a row got no frame")
            }
        } catch {}
    }

    private func handle(_ frame: Data) {
        received += 1
        lastFrame = .now
        misses = 0
        guard let message = try? JSONDecoder().decode(JSONValue.self, from: frame) else {
            log("dropped a \(frame.count)-byte frame from the Guest Service that is not JSON")
            return
        }
        if case .int(let id)? = message["id"], message["method"] == nil {
            let method = methods.removeValue(forKey: id)
            guard pending[id] != nil else {
                if let method { log("dropped a \(frame.count)-byte answer to \(method) (request \(id)), which no longer waits for it") }
                return
            }
            if let error = message["error"], case .int(let code)? = error["code"] {
                var text = ""
                if case .string(let m)? = error["message"] { text = m }
                fail(id, .remote(code: code, message: text))
            } else {
                pending.removeValue(forKey: id)?.resume(returning: message["result"] ?? .null)
            }
        } else if case .string(let method)? = message["method"], message["id"] == nil {
            let params = message["params"] ?? .null
            switch method {
            case "guest.ready":
                // A restarted agent or a reopened port: whatever ran before is not ours to follow any more.
                log("Guest Service sent guest.ready")
                info = nil
                unresponsive = false
                Task { try? await self.establish() }
            case "exec.output":
                if case .int(let pid)? = params["pid"], case .string(let stream)? = params["stream"], case .string(let b64)? = params["data"],
                    let data = Data(base64Encoded: b64)
                {
                    emit(.output(pid: pid, stream: stream, data: data))
                }
            case "exec.exit":
                if case .int(let pid)? = params["pid"], case .int(let code)? = params["code"] { emit(.exit(pid: pid, code: code)) }
            default: break
            }
        }
    }
}
