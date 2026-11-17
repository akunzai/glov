import Foundation
import GlovCore
import Logging
import MCP

/// MCP over stdin/stdout with blocking writes. The SDK's `StdioTransport` makes stdout non-blocking and sleeps 10 ms
/// whenever the pipe is full, so a screenshot reply (about 1.4 MB) took hundreds of milliseconds to leave the process.
actor BlockingStdioTransport: Transport {
    let logger = Logger(label: "glov.mcp.stdio", factory: { _ in SwiftLogNoOpLogHandler() })
    private let input: Int32 = STDIN_FILENO
    private let output: Int32 = STDOUT_FILENO
    /// Blocking writes run here, not on a cooperative thread; serial, so replies leave in the order they were sent.
    private let writer = DispatchQueue(label: "org.glov.mcp.stdout")

    /// Both ends must block: a read that fails with EAGAIN would end the session.
    func connect() async throws {
        for fd in [input, output] {
            let flags = fcntl(fd, F_GETFL)
            if flags >= 0, flags & O_NONBLOCK != 0 { _ = fcntl(fd, F_SETFL, flags & ~O_NONBLOCK) }
        }
    }

    func disconnect() async {}

    func send(_ data: Data) async throws {
        let output = output
        // errno is per thread, so it is read on the thread that wrote.
        let error: Int32? = await Blocking.run(on: writer) { UnixSocket.write(fd: output, data + Data("\n".utf8)) ? nil : errno }
        if let error { throw MCPError.transportError(POSIXError(POSIXErrorCode(rawValue: error) ?? .EIO)) }
    }

    func receive() -> AsyncThrowingStream<Data, any Error> {
        let lines = UnixSocket.lines(fd: input)
        return AsyncThrowingStream { cont in
            let task = Task {
                for await line in lines where !line.isEmpty { cont.yield(line) }
                cont.finish()
            }
            cont.onTermination = { _ in task.cancel() }
        }
    }
}
