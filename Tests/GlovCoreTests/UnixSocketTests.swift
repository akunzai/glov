import Foundation
import Synchronization
import Testing

@testable import GlovCore

@Suite struct UnixSocketTests {
    /// On macOS a thread blocked in `read` on a unix socket can miss the peer's `shutdown` from another thread
    /// (about once in ten thousand). Each worker reads one end of a fresh socket pair to EOF while its own writer thread
    /// writes a few bytes to the other end and shuts it down; with a plain `read` some reader sleeps for good.
    @Test func aReaderSeesEOFAfterThePeersShutdownEveryTime() async {
        let workers = 8, iterations = 25_000
        let finished = DispatchGroup()
        let missed = Mutex<Int>(0)
        for _ in 0..<workers {
            finished.enter()
            Thread.detachNewThread {
                defer { finished.leave() }
                let go = DispatchSemaphore(value: 0), wrote = DispatchSemaphore(value: 0)
                // Handed between the two threads through `go`, which orders the accesses.
                final class Turn: @unchecked Sendable { var peer: Int32 = -1; var stop = false }
                let turn = Turn()
                Thread.detachNewThread {
                    while true {
                        go.wait()
                        if turn.stop { return }
                        _ = write(turn.peer, "HTTP/1.1 200 OK\r\n\r\n", 19)
                        shutdown(turn.peer, SHUT_WR)
                        wrote.signal()
                    }
                }
                var buffer = [UInt8](repeating: 0, count: 4096)
                for _ in 0..<iterations {
                    var pair: [Int32] = [0, 0]
                    guard socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0 else { Issue.record("socketpair: \(errno)"); break }
                    turn.peer = pair[1]
                    go.signal()
                    var total = 0
                    while case let n = buffer.withUnsafeMutableBytes({ UnixSocket.readWhenReadable(pair[0], $0) }), n > 0 { total += n }
                    if total != 19 { missed.withLock { $0 += 1 } }
                    wrote.wait()
                    close(pair[0]); close(pair[1])
                }
                turn.stop = true
                go.signal()
            }
        }
        let result = await Blocking.run { finished.wait(timeout: .now() + 30) }
        #expect(result == .success, "a reader never saw EOF after its peer's shutdown")
        #expect(missed.withLock { $0 } == 0)
    }

    /// A bridge copies `source` to `destination` until EOF, and its peer may end with `shutdown`: a plain `read` there
    /// can hang for good. Every bridge names its input `source`, so a `read(source` outside the helper is new.
    @Test func bridgesReadThroughReadWhenReadable() async throws {
        let sources = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Sources")
        // Scanning every source takes a while; off the cooperative pool, like any long blocking work in a test.
        let offenders = try await Blocking.run { () throws -> [String] in
            let plainRead = try Regex(#"(^|[^A-Za-z_.])((Darwin|Foundation)\.)?read\(source\b"#)
            var offenders: [String] = []
            for case let file as URL in FileManager.default.enumerator(at: sources, includingPropertiesForKeys: nil)! where file.pathExtension == "swift" {
                for (i, line) in try String(contentsOf: file, encoding: .utf8).split(separator: "\n", omittingEmptySubsequences: false).enumerated()
                where line.contains(plainRead) {
                    offenders.append("\(file.lastPathComponent):\(i + 1)")
                }
            }
            return offenders
        }
        #expect(offenders.isEmpty, "use UnixSocket.readWhenReadable in bridge copy loops: \(offenders)")
    }
}
