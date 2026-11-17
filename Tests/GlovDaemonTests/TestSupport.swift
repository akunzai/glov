import Foundation
import GlovCore
import GlovDaemonAPI

private final class BundleToken {}

struct MissingBinary: Error { var name: String }

/// Runs `process` to its exit without holding a cooperative thread: Swift Testing runs every test on that pool, so a
/// test blocked in `waitUntilExit` stalls the others (one thread under `LIBDISPATCH_COOPERATIVE_POOL_STRICT`).
func runToExit(_ process: Process) async throws {
    let (exited, exit) = AsyncStream<Void>.makeStream()
    process.terminationHandler = { _ in exit.yield(); exit.finish() }
    try process.run()
    for await _ in exited {}
}

/// The built `glovd` / `glov` next to the test bundle (the test target depends on both, so `swift test` builds them).
func builtBinary(_ name: String) throws -> String {
    let path = URL(fileURLWithPath: Bundle(for: BundleToken.self).bundlePath).deletingLastPathComponent().appendingPathComponent(name).path
    guard FileManager.default.isExecutableFile(atPath: path) else { throw MissingBinary(name: path) }
    return path
}

/// Runs `body`, which waits for real tools (hdiutil, qemu-img, codesign), off the cooperative pool: a synchronous
/// test runs on a pool thread, and with LIBDISPATCH_COOPERATIVE_POOL_STRICT=1 that is the only one.
func offPool<T: Sendable>(_ body: @escaping () throws -> T) async throws -> T {
    // The test awaits `body` and never calls it again, so handing it to another thread is safe.
    nonisolated(unsafe) let body = body
    return try await Blocking.run { try body() }
}

/// A child `glovd` on a temp root.
final class DaemonProcess {
    let process = Process()
    let client: DaemonClient
    private let (exited, exit) = AsyncStream<Void>.makeStream()

    init(home: GlovHome) throws {
        process.executableURL = URL(fileURLWithPath: try builtBinary("glovd"))
        process.arguments = ["--home", home.path]
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        process.terminationHandler = { [exit] _ in exit.yield(); exit.finish() }
        try process.run()
        client = DaemonClient(socketPath: home.socketPath)
    }

    func waitUntilReady() async throws {
        for _ in 0..<100 {
            if (try? await client.call(Method.vmList)) != nil { return }
            try await Task.sleep(for: .milliseconds(100))
        }
        throw MissingBinary(name: "glovd did not start")
    }

    /// SIGKILL: the daemon cannot clean up, like a crash. Its VMs keep running.
    func crash() async {
        kill(process.processIdentifier, SIGKILL)
        // A termination handler, not waitUntilExit or a semaphore: both block a cooperative thread, and
        // waitUntilExit spins a run loop that can miss the exit while other tests spawn processes (it hung a run).
        for await _ in exited {}
    }
}

/// `glov mcp` as a child process, speaking newline-delimited JSON-RPC on stdio.
final class MCPSession: @unchecked Sendable {  // used from one test at a time
    private let process = Process()
    private let stdin = Pipe()
    private let stdout = Pipe()
    private var lines: AsyncStream<Data>.Iterator
    private var nextID = 1

    init(home: GlovHome, executable: String = "glov", arguments: [String] = ["mcp"]) throws {
        process.executableURL = URL(fileURLWithPath: try builtBinary(executable))
        process.arguments = arguments
        process.environment = ProcessInfo.processInfo.environment.merging(["GLOV_HOME": home.path]) { $1 }
        process.standardInput = stdin
        process.standardOutput = stdout
        process.standardError = FileHandle.nullDevice
        try process.run()
        // Not FileHandle.bytes, whose reads block a cooperative thread; the pipe stays open as long as `stdout`.
        lines = UnixSocket.lines(fd: stdout.fileHandleForReading.fileDescriptor).makeAsyncIterator()
    }

    func terminate() { process.terminate() }

    private func send(_ object: [String: Any]) async throws {
        let data = try JSONSerialization.data(withJSONObject: object) + Data("\n".utf8)
        let input = stdin.fileHandleForWriting
        try await Blocking.run { try input.write(contentsOf: data) }  // a pipe write blocks once the child stops reading
    }

    func notify(_ method: String) async throws { try await send(["jsonrpc": "2.0", "method": method]) }

    /// Sends a request and returns the `result` of its reply.
    func request(_ method: String, _ params: [String: Any] = [:]) async throws -> [String: Any] {
        let id = nextID
        nextID += 1
        try await send(["jsonrpc": "2.0", "id": id, "method": method, "params": params])
        while let line = await lines.next() {
            guard let message = try JSONSerialization.jsonObject(with: line) as? [String: Any], message["id"] as? Int == id else { continue }
            if let error = message["error"] { throw MissingBinary(name: "MCP error: \(error)") }
            return message["result"] as? [String: Any] ?? [:]
        }
        throw MissingBinary(name: "glov mcp closed stdout")
    }

    func initialize() async throws -> [String: Any] {
        let result = try await request("initialize", [
            "protocolVersion": "2025-06-18", "capabilities": [String: Any](),
            "clientInfo": ["name": "glov-tests", "version": "0"],
        ])
        try await notify("notifications/initialized")
        return result
    }
}

/// Runs the built `glov` on `home` with `stdin` (in `directory` when given), and returns its exit status and output.
func runglov(
    _ arguments: [String], home: GlovHome, stdin: String = "", executable: String = "glov", directory: String? = nil
) async throws -> (status: Int32, out: String, err: String) {
    let p = Process()
    if let directory { p.currentDirectoryURL = URL(fileURLWithPath: directory) }
    p.executableURL = URL(fileURLWithPath: try builtBinary(executable))
    p.arguments = arguments
    p.environment = ProcessInfo.processInfo.environment.merging(["GLOV_HOME": home.path]) { $1 }
    let input = Pipe(), out = Pipe(), err = Pipe()
    p.standardInput = input
    p.standardOutput = out
    p.standardError = err
    // A termination handler, not waitUntilExit, which can miss the exit while other tests spawn processes.
    let (exited, exit) = AsyncStream<Void>.makeStream()
    p.terminationHandler = { _ in exit.yield(); exit.finish() }
    try p.run()
    // Pipe reads and writes block, and Task.detached still runs on the cooperative pool.
    let toChild = input.fileHandleForWriting
    // A child that exits before reading its input closes the pipe (EPIPE); its status and output are what the test checks.
    await Blocking.run {
        try? toChild.write(contentsOf: Data(stdin.utf8))
        try? toChild.close()
    }
    async let o = Blocking.run { out.fileHandleForReading.readDataToEndOfFile() }
    async let e = Blocking.run { err.fileHandleForReading.readDataToEndOfFile() }
    let (od, ed) = await (o, e)
    for await _ in exited {}
    return (p.terminationStatus, String(decoding: od, as: UTF8.self), String(decoding: ed, as: UTF8.self))
}
