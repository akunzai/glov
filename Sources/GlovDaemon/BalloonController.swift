import Foundation
import GlovCore
import GlovDaemonAPI

/// The QMP side of a VM's virtio-balloon, so `BalloonController` can be driven without QEMU.
protocol BalloonDevice: Sendable {
    /// Asks the guest to keep `bytes` (QMP `balloon`).
    func setTarget(_ bytes: UInt64) async throws
    /// What the guest has now (QMP `query-balloon` `actual`).
    func actual() async throws -> UInt64
    /// Has QEMU ask the guest for its memory stats every `seconds`.
    func enableStats(every seconds: Int) async throws
    /// The guest's last memory stats; nil when it never sent any.
    func stats() async throws -> BalloonStats?
    /// False once the VM is gone, which ends the controller instead of a retry.
    func isRunning() async -> Bool
}

struct QMPBalloon: BalloonDevice {
    let vm: VirtualMachine
    static let path = "/machine/peripheral/\(VMConfig.balloonDeviceID)"

    func setTarget(_ bytes: UInt64) async throws { _ = try await vm.qmpClient().execute("balloon", .object(["value": .int(Int(bytes))])) }

    func actual() async throws -> UInt64 {
        guard case .int(let actual)? = try await vm.qmpClient().execute("query-balloon")["actual"], actual >= 0 else {
            throw ProvisionError("QMP query-balloon did not report the guest's memory")
        }
        return UInt64(actual)
    }

    func enableStats(every seconds: Int) async throws {
        _ = try await vm.qmpClient().execute(
            "qom-set", .object(["path": .string(Self.path), "property": .string("guest-stats-polling-interval"), "value": .int(seconds)]))
    }

    func isRunning() async -> Bool { await vm.isRunning }

    func stats() async throws -> BalloonStats? {
        BalloonStats(qom: try await vm.qmpClient().execute("qom-get", .object(["path": .string(Self.path), "property": .string("guest-stats")])))
    }
}

/// Where the Mac's memory pressure comes from; a debug build reads `GLOV_DEBUG_MAC_PRESSURE_FILE` instead when it is set,
/// so a test of the daemon can raise pressure without starving the Mac.
enum MacPressureSource {
    static func current() -> MacPressure {
        #if DEBUG
            if let path = ProcessInfo.processInfo.environment["GLOV_DEBUG_MAC_PRESSURE_FILE"],
                let level = try? String(contentsOfFile: path, encoding: .utf8)
            {
                return MacPressure(level: level.trimmingCharacters(in: .whitespacesAndNewlines))
            }
        #endif
        return MacPressure(level: MacResources.memory().pressure)
    }
}

/// Applies a QEMU VM's fixed balloon target when it starts or, in auto, starts it with all its memory and keeps choosing
/// a target (`BalloonPolicy`) until the VM stops. Runs beside the Dispatcher, so a slow QMP answer holds up no other
/// request, and a failed QMP call is retried rather than ending it.
actor BalloonController {
    struct Timing: Sendable {
        /// How often QEMU asks the guest for stats.
        var statsInterval = 2
        /// Between decisions while the guest has memory to spare, and right after it ran short.
        var cycle: Duration = .seconds(5)
        var urgentCycle: Duration = .seconds(1)
        /// The wait after a failed QMP call, doubling up to `maxRetry`.
        var retry: Duration = .seconds(5)
        var maxRetry: Duration = .seconds(30)
    }

    let name: String
    private let device: any BalloonDevice
    private let configured: UInt64
    private let auto: Bool
    private var target: UInt64
    private let pressure: @Sendable () -> MacPressure
    private let now: @Sendable () -> Date
    private let timing: Timing
    private var lastChange: Date?
    private(set) var reason: String
    private(set) var used: UInt64?
    private(set) var lastPressure: MacPressure?
    private var task: Task<Void, Never>?
    private var stopped = false
    /// Auto on a VM that was already running (a daemon restart, or auto turned on): start from what the guest has.
    private let resuming: Bool

    /// `actual`: with `resuming`, what the guest has as the caller last read it, so the target reads right before the
    /// loop reads it again.
    init(
        config: VMConfig, device: any BalloonDevice, resuming: Bool = false, actual: UInt64? = nil, timing: Timing = .init(),
        pressure: @escaping @Sendable () -> MacPressure = MacPressureSource.current, now: @escaping @Sendable () -> Date = Date.init
    ) {
        let configured = config.memoryBytes ?? 0
        name = config.name
        self.device = device
        self.configured = configured
        auto = config.balloonAuto
        // Auto starts with everything: a boot needs more than the guest later uses, and its first stats come late.
        target = config.balloonAuto ? configured : min(config.balloonTarget ?? configured, configured)
        if config.balloonAuto, resuming, let actual { target = min(actual, configured) }
        self.pressure = pressure
        self.now = now
        self.timing = timing
        self.resuming = resuming
        reason = config.balloonAuto ? "waiting for the guest's first memory stats" : "fixed"
    }

    var currentTarget: UInt64 { target }

    func start() {
        guard !stopped, task == nil else { return }
        task = Task { await self.run() }
    }

    func stop() { stopped = true; task?.cancel(); task = nil }

    /// Waits for the loop to end (tests).
    func finished() async { await task?.value }

    private func run() async {
        var backoff = timing.retry
        var ready = false
        while !Task.isCancelled {
            do {
                if !ready {
                    // Handing a guest glovd already shrank all its memory back on every daemon restart would undo
                    // auto for a minute; without an answer, all of it is the safe side.
                    if auto && resuming && lastChange == nil, let actual = try? await device.actual() { target = min(actual, configured) }
                    // Also the full size: QEMU may still hold another target from before a daemon restart.
                    try await device.setTarget(target)
                    guard auto else { return }
                    try await device.enableStats(every: timing.statsInterval)
                    // Starting at the full size counts as a change: the first decrease waits like any other.
                    if lastChange == nil { lastChange = now() }
                    ready = true
                }
                let urgent = try await cycle()
                backoff = timing.retry
                try await Task.sleep(for: urgent ? timing.urgentCycle : timing.cycle)
            } catch is CancellationError {
                return
            } catch {
                guard await device.isRunning() else { return }
                ready = false
                reason = "QMP failed (\(error)), retrying in \(backoff.components.seconds)s; the target stays at \(BalloonPolicy.size(target))"
                DaemonLog.write("balloon \(name): \(error); retrying in \(backoff.components.seconds)s")
                try? await Task.sleep(for: backoff)
                backoff = min(backoff * 2, timing.maxRetry)
            }
        }
    }

    /// One decision; true when the guest was short of memory.
    func cycle() async throws -> Bool {
        let stats = try await device.stats()
        let actual = try await device.actual()
        let level = pressure()
        let clock = now()
        let d = BalloonPolicy.decide(
            stats: stats, actual: actual, configured: configured, pressure: level, target: target, lastChange: lastChange, now: clock)
        reason = d.reason
        used = d.used
        lastPressure = level
        if d.target != target {
            try Task.checkCancellation()
            try await device.setTarget(d.target)
            target = d.target
            lastChange = clock
        }
        return d.urgent
    }
}
