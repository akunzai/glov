import Foundation
import Testing

@testable import GlovCore

@Suite struct BalloonPolicyTests {
    static let G = BalloonPolicy.gib
    static let M = BalloonPolicy.mib
    let now = Date(timeIntervalSince1970: 1_000_000)

    /// A 4G guest that uses `used`, with the balloon holding `4G - actual`.
    func decide(
        used: UInt64, actual: UInt64? = nil, configured: UInt64 = 4 * G, pressure: MacPressure = .normal, target: UInt64? = nil,
        lastChange: Date? = nil, age: TimeInterval = 2
    ) -> BalloonPolicy.Decision {
        let actual = actual ?? configured
        let held = configured - actual
        let stats = BalloonStats(total: configured, available: configured - used - held, updated: now.addingTimeInterval(-age))
        return BalloonPolicy.decide(
            stats: stats, actual: actual, configured: configured, pressure: pressure, target: target ?? actual, lastChange: lastChange, now: now)
    }

    @Test func theGuestsOwnUseLeavesOutWhatTheBalloonHolds() {
        // Measured on Windows 11: the balloon's 1.5G shows as in use, available drops by as much, total stays at -m.
        let stats = BalloonStats(total: 4 * Self.G, available: 840 * Self.M, updated: now)
        #expect(BalloonPolicy.used(stats, actual: 2560 * Self.M, configured: 4 * Self.G) == 4 * Self.G - 840 * Self.M - 1536 * Self.M)
    }

    @Test func normalPressureKeepsUsePlusTwoGigabytesOrHalf() {
        #expect(decide(used: 1 * Self.G).target == 3 * Self.G)
        #expect(decide(used: 6 * Self.G, configured: 16 * Self.G).target == 9 * Self.G, "half of 6G is more than 2G")
        #expect(decide(used: 1 * Self.G).reason == "guest uses 1.00G + 2.00G headroom (Mac pressure normal)")
    }

    @Test func pressureKeepsUsePlusOneGigabyteOrAQuarter() {
        #expect(decide(used: 1536 * Self.M, pressure: .warning).target == 2560 * Self.M)
        #expect(decide(used: 8 * Self.G, configured: 16 * Self.G, pressure: .critical).target == 10 * Self.G)
    }

    @Test func theTargetStaysBetweenTheFloorAndTheVMsMemory() {
        #expect(BalloonPolicy.floor(configured: 4 * Self.G) == 1 * Self.G)
        #expect(BalloonPolicy.floor(configured: 16 * Self.G) == 4 * Self.G)
        #expect(BalloonPolicy.floor(configured: 512 * Self.M) == 512 * Self.M, "never more than the VM has")
        let floored = decide(used: 2 * Self.G, configured: 16 * Self.G, pressure: .critical)
        #expect(floored.target == 4 * Self.G && floored.reason.hasSuffix("kept at the floor of 4.00G"))
        let capped = decide(used: 3 * Self.G, actual: 3 * Self.G + 512 * Self.M, target: 3 * Self.G + 512 * Self.M)
        #expect(capped.target == 4 * Self.G && capped.reason.hasSuffix("capped at the VM's 4.00G"))
    }

    @Test func aGuestRunningShortIsRaisedAtOnce() {
        // 2.5G target, guest now uses 2.2G of it: 300M available, just after a decrease.
        let d = decide(used: 2200 * Self.M, actual: 2560 * Self.M, lastChange: now.addingTimeInterval(-1))
        #expect(d.target == 4 * Self.G && d.urgent)
        #expect(d.reason.hasPrefix("guest had only 0.35G available, raised to all 4.00G at once: "))
        // Straight to the VM's memory, not use + headroom: Windows paged before its stats showed the need.
        let big = decide(used: 5939 * Self.M, actual: 6 * Self.G, configured: 16 * Self.G)
        #expect(big.target == 16 * Self.G && big.urgent)
    }

    @Test func smallChangesAreNotSent() {
        #expect(decide(used: 1 * Self.G + 200 * Self.M, actual: 3 * Self.G).target == 3 * Self.G, "200M up is under the 256M step")
        #expect(decide(used: 1 * Self.G, actual: 3 * Self.G + 200 * Self.M).target == 3 * Self.G + 200 * Self.M, "200M down is under 256M")
        #expect(decide(used: 1 * Self.G, actual: 3 * Self.G + 300 * Self.M).target == 3 * Self.G, "300M down is sent")
        #expect(decide(used: 1 * Self.G + 300 * Self.M, actual: 3 * Self.G).target == 3 * Self.G + 300 * Self.M, "300M up is sent")
    }

    @Test func aDecreaseWaitsAMinuteAfterTheLastChange() {
        let held = decide(used: 512 * Self.M, lastChange: now.addingTimeInterval(-20))
        #expect(held.target == 4 * Self.G)
        #expect(held.reason.hasSuffix("; going down to 2.50G in 40s"))
        #expect(decide(used: 512 * Self.M, lastChange: now.addingTimeInterval(-60)).target == 2560 * Self.M)
        #expect(decide(used: 512 * Self.M).target == 2560 * Self.M, "nothing changed yet")
    }

    @Test func withoutStatsTheTargetStays() {
        let none = BalloonPolicy.decide(
            stats: nil, actual: 3 * Self.G, configured: 4 * Self.G, pressure: .critical, target: 3 * Self.G, lastChange: nil, now: now)
        #expect(none.target == 3 * Self.G && none.used == nil)
        #expect(none.reason == "no memory stats from the guest (is its balloon service running?), so the target stays at 3.00G")
        #expect(decide(used: 512 * Self.M, age: 31).target == 4 * Self.G, "stats older than 30s are not used")
    }

    @Test func statsAreReadFromQEMUsGuestStats() throws {
        // As Windows' balloon service reports them: what it does not track is UINT64_MAX.
        let json = #"""
            {"stats": {"stat-available-memory": 2384115712, "stat-free-memory": 2360139776, "stat-total-memory": 4285779968,
             "stat-disk-caches": 47951872, "stat-oom-kills": 18446744073709551615}, "last-update": 1793787015}
            """#
        let qom = try JSONDecoder().decode(JSONValue.self, from: Data(json.utf8))
        #expect(BalloonStats(qom: qom) == BalloonStats(total: 4_285_779_968, available: 2_384_115_712, updated: Date(timeIntervalSince1970: 1_793_787_015)))
        // No balloon service: QEMU answers with no stats and last-update 0.
        let empty = try JSONDecoder().decode(JSONValue.self, from: Data(#"{"stats": {"stat-total-memory": 18446744073709551615}, "last-update": 0}"#.utf8))
        #expect(BalloonStats(qom: empty) == nil)
    }
}
