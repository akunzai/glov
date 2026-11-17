import Foundation

/// What a Windows guest's balloon service last reported (QMP `qom-get` `guest-stats` on the balloon device).
public struct BalloonStats: Sendable, Equatable {
    /// `stat-total-memory`: the VM's `-m`, less firmware reservations. Memory the balloon holds still counts here.
    public var total: UInt64
    /// `stat-available-memory`: what the guest can hand out without paging. The balloon's memory is not available.
    public var available: UInt64
    /// `last-update`: when QEMU received them (host clock).
    public var updated: Date

    public init(total: UInt64, available: UInt64, updated: Date) {
        self.total = total; self.available = available; self.updated = updated
    }

    /// Reads QEMU's `guest-stats` object; nil when the guest has never reported (no balloon service) or left a field out.
    /// QEMU reports a statistic the guest does not support as UINT64_MAX.
    public init?(qom: JSONValue) {
        guard case .int(let updated)? = qom["last-update"], updated > 0, let stats = qom["stats"],
            case .int(let total)? = stats["stat-total-memory"], case .int(let available)? = stats["stat-available-memory"],
            total > 0, available >= 0, available <= total
        else { return nil }
        self.init(total: UInt64(total), available: UInt64(available), updated: Date(timeIntervalSince1970: TimeInterval(updated)))
    }
}

/// The Mac's memory pressure as the kernel grades it.
public enum MacPressure: String, Sendable, Equatable, Codable {
    case normal, warning, critical

    public init(level: String) { self = MacPressure(rawValue: level) ?? .normal }
}

/// Picks the memory a Windows guest keeps in `glov vm balloon <vm> auto`: what the guest uses plus headroom for what it
/// is about to use, with less headroom while the Mac is short of memory. Pure, so every branch is tested.
public enum BalloonPolicy {
    public static let gib: UInt64 = 1 << 30
    public static let mib: UInt64 = 1 << 20

    /// Headroom while the Mac has memory to spare: room for a build or a browser to start without waiting on the balloon.
    public static func headroom(used: UInt64, pressure: MacPressure) -> UInt64 {
        pressure == .normal ? max(2 * gib, used / 2) : max(gib, used / 4)
    }

    /// Never less than 1G, where Windows pages without pause, nor a quarter of `-m`, so a big VM is never squeezed
    /// further than its driver can hand back in a few seconds.
    public static func floor(configured: UInt64) -> UInt64 { min(configured, max(gib, configured / 4)) }

    /// Below this much available the guest is about to page: the target goes straight to the VM's full memory, ignoring
    /// the steps below. Windows pages before its stats show the real need, so use plus headroom would fall short; the
    /// usual decrease brings it down again.
    public static let lowAvailable: UInt64 = 512 * mib
    /// A smaller change is not worth asking the guest for, either way; a guest whose use wobbles keeps one target.
    public static let step: UInt64 = 256 * mib
    /// How long after any change the target may go down again, so a burst that just ended may come back without the
    /// target going down and up with it. Going up never waits.
    public static let decreaseInterval: TimeInterval = 60
    /// Stats older than this come from a balloon service that stopped reporting.
    public static let staleAfter: TimeInterval = 30

    public struct Decision: Sendable, Equatable {
        /// The target to keep; differs from the current one only when it should be sent.
        public var target: UInt64
        public var reason: String
        /// The guest's own use (without the balloon's memory), when its stats were usable.
        public var used: UInt64?
        /// True when the guest was short of memory: check again soon.
        public var urgent: Bool = false
    }

    /// The guest's own use: what it does not have available, less what the balloon holds (`configured - actual`),
    /// which Windows counts as in use.
    public static func used(_ stats: BalloonStats, actual: UInt64, configured: UInt64) -> UInt64 {
        used(total: stats.total, available: stats.available, actual: actual, configured: configured)
    }

    /// `used` from any report of the guest's total and available memory (`guest.stats` too).
    public static func used(total: UInt64, available: UInt64, actual: UInt64, configured: UInt64) -> UInt64 {
        let held = configured > actual ? configured - actual : 0
        let inUse = total > available ? total - available : 0
        return inUse > held ? inUse - held : 0
    }

    public static func decide(
        stats: BalloonStats?, actual: UInt64, configured: UInt64, pressure: MacPressure, target current: UInt64,
        lastChange: Date?, now: Date
    ) -> Decision {
        guard let stats, now.timeIntervalSince(stats.updated) <= staleAfter else {
            return .init(
                target: current,
                reason: "no memory stats from the guest (is its balloon service running?), so the target stays at \(size(current))")
        }
        let used = used(stats, actual: actual, configured: configured)
        let room = headroom(used: used, pressure: pressure)
        let low = floor(configured: configured)
        let wanted = min(configured, max(low, used + room))
        var why = "guest uses \(size(used)) + \(size(room)) headroom (Mac pressure \(pressure.rawValue))"
        if wanted == low && used + room < low { why += ", kept at the floor of \(size(low))" }
        if wanted == configured && used + room > configured { why += ", capped at the VM's \(size(configured))" }

        if stats.available < lowAvailable && current < configured {
            return .init(
                target: configured, reason: "guest had only \(size(stats.available)) available, raised to all \(size(configured)) at once: " + why,
                used: used, urgent: true)
        }
        if wanted > current {
            if wanted - current >= step { return .init(target: wanted, reason: why, used: used) }
            return .init(target: current, reason: why, used: used)
        }
        if current - wanted < step { return .init(target: current, reason: why, used: used) }
        if let lastChange, now.timeIntervalSince(lastChange) < decreaseInterval {
            let wait = Int((decreaseInterval - now.timeIntervalSince(lastChange)).rounded(.up))
            return .init(target: current, reason: why + "; going down to \(size(wanted)) in \(wait)s", used: used)
        }
        return .init(target: wanted, reason: why, used: used)
    }

    public static func size(_ bytes: UInt64) -> String { String(format: "%.2fG", Double(bytes) / Double(gib)) }
}
