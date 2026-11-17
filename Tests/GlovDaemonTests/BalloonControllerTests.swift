import Foundation
import GlovCore
import Testing

@testable import GlovDaemon

/// A balloon whose guest uses `used` bytes and gives up or takes back memory at once.
final class FakeBalloon: BalloonDevice, @unchecked Sendable {
    private let lock = NSLock()
    private var _actual: UInt64
    private var _used: UInt64
    private var _targets: [UInt64] = []
    var reports: Bool
    /// The next this many calls fail, as a QMP timeout would.
    var failures = 0
    var running = true
    let configured: UInt64

    init(configured: UInt64, used: UInt64, reports: Bool = true) {
        self.configured = configured; _actual = configured; _used = used; self.reports = reports
    }

    var targets: [UInt64] { lock.withLock { _targets } }
    func use(_ bytes: UInt64) { lock.withLock { _used = bytes } }
    /// What a balloon a previous daemon inflated leaves the guest with.
    func hold(_ actual: UInt64) { lock.withLock { _actual = actual } }

    struct Failure: Error {}
    private func fail() throws {
        try lock.withLock { if failures > 0 { failures -= 1; throw Failure() } }
    }

    func setTarget(_ bytes: UInt64) async throws { try fail(); lock.withLock { _targets.append(bytes); _actual = bytes } }
    func actual() async throws -> UInt64 { try fail(); return lock.withLock { _actual } }
    func enableStats(every seconds: Int) async throws { try fail() }
    func isRunning() async -> Bool { running }
    func stats() async throws -> BalloonStats? {
        try fail()
        return lock.withLock {
            guard reports else { return nil }
            let held = configured - _actual
            let available = _used + held > configured ? 0 : configured - _used - held
            return BalloonStats(total: configured, available: available, updated: Date())
        }
    }
}

/// A clock tests move by hand.
final class FakeClock: @unchecked Sendable {
    private let lock = NSLock()
    private var _now = Date(timeIntervalSince1970: 1_000_000)
    var now: Date { lock.withLock { _now } }
    func advance(_ seconds: TimeInterval) { lock.withLock { _now += seconds } }
}

@Suite struct BalloonControllerTests {
    static let G = BalloonPolicy.gib
    static let M = BalloonPolicy.mib
    static let fast = BalloonController.Timing(
        statsInterval: 1, cycle: .milliseconds(20), urgentCycle: .milliseconds(5), retry: .milliseconds(20), maxRetry: .milliseconds(50))

    func config(auto: Bool, target: UInt64? = nil) -> VMConfig {
        VMConfig(name: "win", directory: "/vm/win", memory: "4G", firmwareCode: "/fw", balloon: true, balloonAuto: auto, balloonTarget: target)
    }

    @Test func aFixedTargetIsAppliedOnceAtStart() async {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G)
        let c = BalloonController(config: config(auto: false, target: 3 * Self.G), device: device)
        await c.start()
        await c.finished()
        #expect(device.targets == [3 * Self.G])
    }

    @Test func autoStartsWithAllTheMemoryAndShrinksOnlyAfterTheUsualWait() async throws {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G)
        let clock = FakeClock()
        // A target left from before is not where an auto VM starts.
        let c = BalloonController(config: config(auto: true, target: 2 * Self.G), device: device, timing: Self.fast, now: { clock.now })
        await c.start()
        await eventually { await c.reason.hasSuffix("going down to 3.00G in 60s") }
        #expect(device.targets == [4 * Self.G])
        clock.advance(60)
        await eventually { device.targets.last == 3 * Self.G }
        #expect(device.targets == [4 * Self.G, 3 * Self.G])
        await c.stop()
    }

    @Test func afterADaemonRestartAutoCarriesOnFromWhatTheGuestHas() async throws {
        let device = FakeBalloon(configured: 4 * Self.G, used: 512 * Self.M)
        device.hold(3 * Self.G)
        let clock = FakeClock()
        let c = BalloonController(config: config(auto: true), device: device, resuming: true, timing: Self.fast, now: { clock.now })
        await c.start()
        await eventually { await c.reason.hasSuffix("going down to 2.50G in 60s") }
        #expect(device.targets == [3 * Self.G], "not all 4G back")
        clock.advance(60)
        await eventually { device.targets.last == 2560 * Self.M }
        await c.stop()
    }

    @Test func autoTurnedOnReportsWhatTheGuestHasBeforeTheLoopRuns() async {
        let device = FakeBalloon(configured: 4 * Self.G, used: 512 * Self.M)
        let c = BalloonController(config: config(auto: true), device: device, resuming: true, actual: 3 * Self.G, timing: Self.fast)
        #expect(await c.currentTarget == 3 * Self.G, "not all 4G")
        let fresh = BalloonController(config: config(auto: true), device: device, actual: 3 * Self.G, timing: Self.fast)
        #expect(await fresh.currentTarget == 4 * Self.G, "a VM that just started gets all its memory")
    }

    @Test func aResumeThatCannotReadTheGuestStartsWithAllTheMemory() async throws {
        let device = FakeBalloon(configured: 4 * Self.G, used: 512 * Self.M)
        device.hold(3 * Self.G)
        device.failures = 1  // query-balloon
        let c = BalloonController(config: config(auto: true), device: device, resuming: true, timing: Self.fast)
        await c.start()
        await eventually { device.targets.first != nil }
        #expect(device.targets.first == 4 * Self.G)
        await c.stop()
    }

    @Test func aGuestThatRunsShortGetsAllItsMemoryAtOnce() async throws {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G)
        let c = BalloonController(config: config(auto: true), device: device)
        #expect(try await c.cycle() == false)
        #expect(device.targets == [3 * Self.G])
        device.use(2800 * Self.M)  // 272M available under the 3G target
        #expect(try await c.cycle() == true, "asks for the quick cycle")
        #expect(device.targets.last == 4 * Self.G, "the full -m, not use + headroom (3.73G)")
        let reason = await c.reason
        #expect(reason.hasPrefix("guest had only 0.27G available, raised to all 4.00G at once"), "\(reason)")
    }

    @Test func aFailedQMPCallIsRetriedAndAutoCarriesOn() async throws {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G)
        let clock = FakeClock()
        let c = BalloonController(config: config(auto: true), device: device, timing: Self.fast, now: { clock.now })
        device.failures = 1  // the first call: the start's setTarget
        await c.start()
        await eventually { await c.reason.hasSuffix("going down to 3.00G in 60s") }
        #expect(device.targets == [4 * Self.G], "sent again after the failure")
        device.failures = 3  // a few calls of the next cycles, as across a QMP reconnect
        clock.advance(60)
        await eventually { await c.reason.hasPrefix("QMP failed") }
        await eventually { device.targets.last == 3 * Self.G }
        await eventually { await c.reason == "guest uses 1.00G + 2.00G headroom (Mac pressure normal)" }
        await c.stop()
    }

    @Test func aVMThatIsGoneEndsTheController() async {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G)
        device.running = false
        device.failures = 1
        let c = BalloonController(config: config(auto: true), device: device, timing: Self.fast)
        await c.start()
        await c.finished()
        #expect(device.targets.isEmpty)
    }

    @Test func aStoppedControllerNeverStarts() async {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G)
        let c = BalloonController(config: config(auto: false, target: 3 * Self.G), device: device)
        await c.stop()
        await c.start()
        await c.finished()
        #expect(device.targets.isEmpty)
    }

    @Test func pressureIsReadEachCycle() async throws {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G)
        let c = BalloonController(config: config(auto: true), device: device, pressure: { .warning })
        #expect(try await c.cycle() == false)
        #expect(device.targets == [2 * Self.G], "use + 1G under pressure")
        #expect(await c.lastPressure == .warning)
    }

    @Test func withoutStatsTheGuestIsNotShrunk() async throws {
        let device = FakeBalloon(configured: 4 * Self.G, used: Self.G, reports: false)
        let c = BalloonController(config: config(auto: true), device: device)
        _ = try await c.cycle()
        #expect(device.targets.isEmpty)
        let reason = await c.reason
        #expect(reason.hasPrefix("no memory stats from the guest"))
    }
}
