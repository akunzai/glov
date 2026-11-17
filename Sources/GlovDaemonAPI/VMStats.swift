import Foundation
import GlovCore

extension Results {
    /// `vm.stats`: the Mac's memory and one row per running VM.
    public struct Stats: Codable, Sendable, Equatable {
        public var mac: MacMemory
        public var vms: [VMStats]
        public init(mac: MacMemory, vms: [VMStats]) { self.mac = mac; self.vms = vms }
    }

    /// One running VM, from the Mac's side and, when its Guest Service answers, from the guest's. Sizes are bytes.
    public struct VMStats: Codable, Sendable, Equatable {
        public var name: String
        public var engine: String
        public var cpus: Int
        /// The configured memory size.
        public var memory: UInt64
        /// The Mac process the VM runs in: qemu, or the Virtualization.framework XPC service of a VZ guest.
        public var pid: Int32?
        /// CPU use of that process over the sample, `top`'s way: 100 is one Mac core.
        public var cpuPercent: Double?
        /// Guest memory in Mac RAM and compressed or swapped out, together at most `memory`, or `balloonMemory` when
        /// known (see `MemoryOnMac`).
        public var memoryResident: UInt64?
        public var memoryCompressed: UInt64?
        /// The system disk image: blocks allocated on the Mac, and the size the guest sees.
        public var diskAllocated: UInt64?
        public var diskSize: UInt64?
        /// Why some Mac-side numbers are missing.
        public var macProblem: String?
        /// QEMU with a balloon device: what the guest has now (`query-balloon` `actual`), at most `memory`; nil when QEMU
        /// did not answer.
        public var balloonMemory: UInt64?
        /// "auto" or "fixed" (`glov vm balloon`), or nil while the balloon is left at all of `memory`.
        public var balloonMode: String?
        public var guest: GuestStats?
        /// The guest's own memory use: what it does not have available, less what the balloon holds, which the guest
        /// counts as in use (`BalloonPolicy.used`). Nil without `guest`.
        public var guestMemoryUsed: UInt64?
        /// Why `guest` is missing: the Guest Service is not ready, too old, or failed to answer.
        public var guestProblem: String?
        public init(name: String, engine: String, cpus: Int, memory: UInt64) {
            self.name = name; self.engine = engine; self.cpus = cpus; self.memory = memory
        }
    }

    /// `guest.stats` (docs/guest-protocol.md): the guest's own view.
    public struct GuestStats: Codable, Sendable, Equatable {
        public var memoryTotal: UInt64
        public var memoryAvailable: UInt64
        public var diskPath: String
        public var diskTotal: UInt64
        public var diskUsed: UInt64
        public var diskAvailable: UInt64
        /// 1, 5 and 15 minute load averages; nil on Windows.
        public var load: [Double]?
        /// Percentage of all guest CPUs busy over a 0.5 s sample.
        public var cpuBusy: Double
        public init(
            memoryTotal: UInt64, memoryAvailable: UInt64, diskPath: String, diskTotal: UInt64, diskUsed: UInt64, diskAvailable: UInt64,
            load: [Double]? = nil, cpuBusy: Double = 0
        ) {
            self.memoryTotal = memoryTotal; self.memoryAvailable = memoryAvailable; self.diskPath = diskPath; self.diskTotal = diskTotal
            self.diskUsed = diskUsed; self.diskAvailable = diskAvailable; self.load = load; self.cpuBusy = cpuBusy
        }
    }
}

/// A VM's memory on the Mac, against its configured size. macOS counts guest memory that HVF maps into the
/// hypervisor process twice: a 16 GiB QEMU guest showed a 31 GiB footprint, while vmmap put its guest RAM region at
/// 16 GiB. So when resident plus compressed exceed the configured size, both are scaled down together to it: the split
/// between RAM and compressed-or-swapped stays, and a VM never reads as using more than it has.
public struct MemoryOnMac: Equatable, Sendable {
    public var resident: UInt64
    public var compressed: UInt64
    public init(resident: UInt64, compressed: UInt64, configured: UInt64) {
        let total = Double(resident) + Double(compressed)
        let scale = total > Double(configured) ? Double(configured) / total : 1
        self.resident = UInt64(Double(resident) * scale)
        self.compressed = UInt64(Double(compressed) * scale)
    }
}

/// When starting a VM risks starving the Mac of memory.
public enum MemoryBudget {
    /// Running VMs may reserve this share of the Mac's memory before a start warns. A 16 GiB Windows guest and an
    /// 8 GiB Engine (3/4 of a 32 GiB Mac) stalled the Engine with other apps open; 16 + 4 GiB stays under it.
    public static let share = 2.0 / 3

    /// The warning for starting `starting` while `running` run, or nil. Sizes are configured memory in bytes.
    public static func warning(starting: (name: String, memory: UInt64), running: [(name: String, memory: UInt64)], mac: MacMemory) -> String? {
        let total = running.reduce(starting.memory) { $0 + $1.memory }
        let over = Double(total) > Double(mac.physical) * share
        let note = pressureNote(mac)
        guard over || note != nil else { return nil }
        var text = "warning: starting \(starting.name) (\(gib(starting.memory)))"
        if !running.isEmpty { text += " with " + running.map { "\($0.name) (\(gib($0.memory)))" }.joined(separator: ", ") + " running" }
        if over { text += " reserves \(gib(total)) of this Mac's \(gib(mac.physical)), more than 2/3" }
        if let note { text += (over ? ", and " : " while ") + note }
        return text + "; macOS may compress and swap guest memory until the VMs stall. "
            + "Stop a VM or lower its memory with 'glov vm configure <vm> --memory', and watch 'glov vm stats'"
    }

    /// "the Mac is under memory pressure (...)" while it is, else nil.
    public static func pressureNote(_ mac: MacMemory) -> String? {
        guard mac.pressure != "normal" else { return nil }
        return "the Mac is under memory pressure (\(mac.pressure); swap \(number(mac.swapUsed)) of \(gib(mac.swapTotal)) used)"
    }

    static func gib(_ bytes: UInt64) -> String { number(bytes) + " GiB" }
    static func number(_ bytes: UInt64) -> String { String(format: "%.1f", Double(bytes) / Double(1 << 30)) }
}
