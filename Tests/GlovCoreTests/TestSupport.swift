import Foundation

@testable import GlovCore

/// A short directory for unix sockets (macOS sun_path is 104 bytes); the default temp dirs are too long.
func shortTempDir() throws -> String {
    var template = Array("/tmp/gp.XXXXXX".utf8CString)
    guard let path = mkdtemp(&template) else { throw CocoaError(.fileWriteUnknown) }
    return String(cString: path)
}

/// Runs `process` to its exit without holding a cooperative thread: Swift Testing runs every test on that pool, so a
/// test blocked in `waitUntilExit` stalls the others (one thread under `LIBDISPATCH_COOPERATIVE_POOL_STRICT`).
func runToExit(_ process: Process) async throws {
    let (exited, exit) = AsyncStream<Void>.makeStream()
    process.terminationHandler = { _ in exit.yield(); exit.finish() }
    try process.run()
    for await _ in exited {}
}

/// In-process QMP server for one client: greets, then answers
/// `qmp_capabilities`/`echo` with `{}`, `fail` with an error, `shutdown` with a SHUTDOWN event then `{}`.
/// `received` records every command name.
final class FakeQMPServer: @unchecked Sendable {
    let path: String
    private let listener: Int32
    private let lock = NSLock()
    private var _received: [String] = []
    var received: [String] { lock.withLock { _received } }

    init(dir: String) throws {
        path = dir + "/qmp.sock"
        var addr = UnixSocket.address(path)!
        listener = socket(AF_UNIX, SOCK_STREAM, 0)
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard rc == 0, listen(listener, 1) == 0 else { throw CocoaError(.fileWriteUnknown) }
        Thread.detachNewThread { [self] in serve() }
    }

    private func send(_ fd: Int32, _ json: String) { _ = UnixSocket.write(fd: fd, Data((json + "\n").utf8)) }

    private func serve() {
        let fd = accept(listener, nil, nil)
        guard fd >= 0 else { return }
        send(fd, #"{"QMP":{"version":{"qemu":{"major":11,"minor":1,"micro":2}},"capabilities":["oob"]}}"#)
        var buf = [UInt8](repeating: 0, count: 65536)
        var acc = Data()
        while true {
            let n = read(fd, &buf, buf.count)
            if n <= 0 { break }
            acc.append(contentsOf: buf[..<n])
            while let nl = acc.firstIndex(of: 10) {
                let line = Data(acc[acc.startIndex..<nl])
                acc.removeSubrange(acc.startIndex...nl)
                guard let msg = try? JSONDecoder().decode(JSONValue.self, from: line),
                    case .string(let cmd)? = msg["execute"], case .int(let id)? = msg["id"]
                else { continue }
                lock.withLock { _received.append(cmd) }
                switch cmd {
                case "fail": send(fd, #"{"error":{"class":"GenericError","desc":"boom"},"id":\#(id)}"#)
                case "shutdown":
                    send(fd, #"{"event":"SHUTDOWN","data":{"guest":true,"reason":"guest-shutdown"},"timestamp":{"seconds":1,"microseconds":2}}"#)
                    send(fd, #"{"return":{},"id":\#(id)}"#)
                case "snapshot-save": send(fd, #"{"return":{},"id":\#(id)}"#)
                case "query-jobs": close(fd); close(listener); return  // QEMU dies mid-job
                case "hangup": close(fd); close(listener); return
                default: send(fd, #"{"return":{"cmd":"\#(cmd)"},"id":\#(id)}"#)
                }
            }
        }
        close(fd)
        close(listener)
    }
}
