import Foundation

/// What the daemon needs from a running VM whichever engine runs it. Engine-specific tools (each engine's `console()`,
/// qcow2 snapshots) stay on the concrete type, so a caller asks for it and gets `GlovError.unsupported` otherwise.
public protocol Machine: Actor {
    nonisolated var config: VMConfig { get }
    var isRunning: Bool { get }
    /// The engine's own process (qemu), or nil when the VM runs inside this process (VZ).
    var processID: Int32? { get }
    /// True once the guest's CPUs run (not paused, not still loading a snapshot).
    func guestIsRunning() async -> Bool
    /// Asks the guest to power down, forces it off after `timeout` (at once when `timeout` is zero) and returns when it is off.
    func stop(timeout: Duration) async
}

/// A machine the host reaches over vsock (VZ): `connect` opens a connection to a Guest Service port.
public protocol VsockMachine: Machine {
    /// A blocking file descriptor the caller owns.
    func connect(port: UInt32) async throws -> Int32
}

extension VMConfig.Engine {
    /// The error for a tool this engine does not implement yet.
    public func unsupported(_ what: String) -> GlovError {
        .unsupported("\(what): not supported for \(self == .vz ? "VZ (Linux)" : rawValue) VMs yet")
    }
}

extension VirtualMachine: Machine {
    public func guestIsRunning() async -> Bool {
        guard let status = try? await qmpClient().execute("query-status") else { return false }
        return status["status"] == "running"
    }
}

/// Keeps the process out of App Nap while any in-process VM runs: the vCPU threads of a VZ guest live in `glovd`,
/// and an app-napped process gets its threads throttled. Counted, so nested holders share one activity.
public final class ActivityAssertion: @unchecked Sendable {
    public static let shared = ActivityAssertion(
        begin: {
            ProcessInfo.processInfo.beginActivity(
                options: [.userInitiated, .latencyCritical], reason: "glov is running a virtual machine")
        },
        end: { ProcessInfo.processInfo.endActivity($0) })

    private let lock = NSLock()
    private var count = 0
    private var token: (any NSObjectProtocol)?
    private let begin: @Sendable () -> any NSObjectProtocol
    private let end: @Sendable (any NSObjectProtocol) -> Void

    public init(begin: @escaping @Sendable () -> any NSObjectProtocol, end: @escaping @Sendable (any NSObjectProtocol) -> Void) {
        self.begin = begin
        self.end = end
    }

    public var isHeld: Bool { lock.withLock { token != nil } }

    public func acquire() {
        lock.withLock {
            count += 1
            if token == nil { token = begin() }
        }
    }

    public func release() {
        lock.withLock {
            guard count > 0 else { return }
            count -= 1
            if count == 0, let t = token {
                end(t)
                token = nil
            }
        }
    }
}
