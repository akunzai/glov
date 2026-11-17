import Foundation
import GlovCore
import GlovDaemonAPI

public enum DaemonError: Error, CustomStringConvertible {
    case alreadyRunning(String)
    public var description: String {
        switch self { case .alreadyRunning(let p): "another glovd is already listening on \(p)" }
    }
}

/// One accepted client. The fd closes when the read loop and every in-flight request are done with it.
private final class Connection: @unchecked Sendable {
    let fd: Int32
    /// Serial, so responses never interleave; a client that stops reading blocks this queue, not a cooperative thread.
    private let writer = DispatchQueue(label: "org.glov.daemon-connection")
    init(fd: Int32) { self.fd = fd }
    deinit { close(fd) }

    func send(_ response: RPCResponse) async {
        guard let data = try? JSONEncoder().encode(response) else { return }
        _ = await Blocking.run(on: writer) { UnixSocket.write(fd: self.fd, data + Data("\n".utf8)) }
    }
}

/// JSON-RPC over `<home>/glovd.sock` (mode 0600), newline-delimited, requests handled concurrently.
public final class DaemonServer: Sendable {
    private let home: GlovHome
    private let dispatcher: Dispatcher
    private let listener: UnixSocket.Listener

    /// Binds the socket. A leftover socket file is replaced only if nothing answers on it.
    public init(home: GlovHome, dispatcher: Dispatcher) throws {
        self.home = home
        self.dispatcher = dispatcher
        try FileManager.default.createDirectory(atPath: home.path, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        if let probe = try? UnixSocket.connect(path: home.socketPath) {
            close(probe)
            throw DaemonError.alreadyRunning(home.socketPath)
        }
        unlink(home.socketPath)
        listener = UnixSocket.Listener(fd: try UnixSocket.listen(path: home.socketPath))
    }

    /// Accepts clients on a background thread until `stop()`. A connection that starts with `SSHRelay.header` and a
    /// space is an SSH relay stream; any other is JSON-RPC.
    public func start() {
        let dispatcher = dispatcher
        listener.start { fd in
            var one: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
            // Peeked on a thread of its own, so a client that sends nothing holds up no other.
            Thread.detachNewThread {
                if Self.startsWithRelayHeader(fd) {
                    Self.relaySSH(fd, dispatcher)
                } else {
                    Self.serveRPC(fd, dispatcher)
                }
            }
        }
    }

    private static func serveRPC(_ fd: Int32, _ dispatcher: Dispatcher) {
            let connection = Connection(fd: fd)
            Task {
                let leaving = Leaving()
                for await line in UnixSocket.lines(fd: fd) {
                    let request = (try? JSONDecoder().decode(RPCRequest.self, from: line)).flatMap { $0.jsonrpc == "2.0" ? $0 : nil }
                    let task = Task {
                        let response: RPCResponse
                        if let request {
                            response = await dispatcher.handle(request)
                        } else {
                            response = RPCResponse(id: .null, error: RPCError(code: RPCError.parseError, message: "not a JSON-RPC 2.0 request"))
                        }
                        await connection.send(response)
                    }
                    if let request, Self.cancelledWhenClientLeaves.contains(request.method) { await leaving.track(task) }
                }
                await leaving.cancelAll()
            }
    }

    /// Peeks, consuming nothing, until the first bytes match `SSHRelay.header` and a space or stop matching it.
    private static func startsWithRelayHeader(_ fd: Int32) -> Bool {
        let prefix = Array((SSHRelay.header + " ").utf8)
        var buffer = [UInt8](repeating: 0, count: prefix.count)
        for _ in 0..<100 {
            let n = recv(fd, &buffer, buffer.count, MSG_PEEK)
            guard n > 0, buffer.prefix(n).elementsEqual(prefix.prefix(n)) else { return false }
            if n == prefix.count { return true }
            usleep(10_000)  // a header split across writes; the relay client sends it in one
        }
        return false
    }

    /// Reads `SSHRelay.header <vm>`, opens the VM's SSH port and pipes the two until both directions end.
    @available(*, noasync, message: "blocks on sockets; run it on a thread of its own")
    private static func relaySSH(_ fd: Int32, _ dispatcher: Dispatcher) {
        defer { close(fd) }
        var tv = timeval(tv_sec: 10, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        // Byte by byte, so nothing after the line (ssh's first bytes) is consumed here.
        let line = SSHRelay.readLine(fd)
        var none = timeval()
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &none, socklen_t(MemoryLayout<timeval>.size))
        let words = line?.split(separator: " ", omittingEmptySubsequences: false) ?? []
        guard words.count == 2, words[0] == SSHRelay.header else {
            _ = EgressProxy.writeAll(fd, Data("error: expected \(SSHRelay.header) <vm>\n".utf8))
            return
        }
        // Written before the signal and read after the wait, so the semaphore orders the two.
        final class Opened: @unchecked Sendable { var result: Result<Int32, any Error> = .failure(CancellationError()) }
        let opened = Opened(), done = DispatchSemaphore(value: 0)
        let vm = String(words[1])
        Task {
            do { opened.result = .success(try await dispatcher.openSSH(vm)) } catch { opened.result = .failure(error) }
            done.signal()
        }
        done.wait()
        switch opened.result {
        case .failure(let error):
            _ = EgressProxy.writeAll(fd, Data("error: \(error)\n".utf8))
        case .success(let guest):
            defer { close(guest) }
            guard EgressProxy.writeAll(fd, Data("ok\n".utf8)) else { return }
            _ = EgressProxy.bridge(fd, guest)
        }
    }

    /// Requests whose work only their client wants: when it disconnects (a killed `glov share sync`), they are
    /// cancelled instead of running on, holding the share, until the guest answers or times out. Everything else
    /// (VM start and stop, Guest Service update) finishes, so that it never stops halfway.
    static let cancelledWhenClientLeaves: Set<String> = [Method.shareSync]

    /// Stops accepting and removes the socket. VMs keep running.
    public func stop() {
        unlink(home.socketPath)
        listener.stop()
    }
}

/// One connection's requests that are cancelled when the client leaves.
private actor Leaving {
    private var tasks: [Task<Void, Never>] = []
    func track(_ task: Task<Void, Never>) { tasks.append(task) }
    func cancelAll() { for t in tasks { t.cancel() } }
}
