import Foundation
import GlovCore
import GlovDaemonAPI

/// `vm.stats`: measures running VMs from the Mac's side (their process, their disk image) while their Guest Services
/// measure them from inside, and puts the two together against each VM's configured size.
enum StatsCollector {
    struct Subject: Sendable {
        var config: VMConfig
        /// The VM's Mac process; nil with `macProblem` saying why.
        var pid: Int32?
        var macProblem: String?
        /// The guest's `guest.stats`, or why there is none.
        var guest: @Sendable () async -> (Results.GuestStats?, String?)
        /// A QEMU VM's balloon: what the guest has now, or nil when QEMU does not answer.
        var balloon: (@Sendable () async -> UInt64?)?
        var balloonMode: String?
    }

    /// A QMP call cannot be cancelled: one QEMU does not answer is left behind after this long.
    static let balloonTimeout: Duration = .seconds(2)

    /// Takes `interval` (the CPU sample); the Guest Service calls run meanwhile.
    static func collect(_ subjects: [Subject], mac: MacMemory, interval: Duration = .seconds(1)) async -> Results.Stats {
        await withTaskGroup(of: (Int, Results.GuestStats?, String?, UInt64?).self) { group in
            for (i, s) in subjects.enumerated() {
                group.addTask {
                    async let balloon = s.balloon == nil ? nil : answer(within: balloonTimeout) { await s.balloon?() }
                    let (g, p) = await s.guest()
                    return (i, g, p, await balloon)
                }
            }
            let first = subjects.map { $0.pid.flatMap(MacResources.usage(of:)) }
            let start = ContinuousClock.now
            try? await Task.sleep(for: interval)
            let seconds = Double((ContinuousClock.now - start) / .milliseconds(1)) / 1000
            let measured = subjects.enumerated().map { i, s in row(s, before: first[i], seconds: seconds) }
            var rows = measured.map(\.row)
            for await (i, guest, problem, balloon) in group {
                if let usage = measured[i].usage {
                    // A guest with a balloon has its amount, not all of `memory`.
                    let memory = MemoryOnMac(resident: usage.resident, compressed: usage.compressed ?? 0, configured: min(rows[i].memory, balloon ?? .max))
                    rows[i].memoryResident = memory.resident
                    rows[i].memoryCompressed = usage.compressed == nil ? nil : memory.compressed
                }
                rows[i].guest = guest
                rows[i].guestProblem = problem
                rows[i].balloonMemory = balloon
                rows[i].guestMemoryUsed = guest.map { guestUsed($0, balloon: balloon, configured: rows[i].memory) }
            }
            return .init(mac: mac, vms: rows)
        }
    }

    /// The row without its memory, which waits for the balloon's amount, and the process's use to place it from.
    private static func row(_ s: Subject, before: ProcessUsage?, seconds: Double) -> (row: Results.VMStats, usage: ProcessUsage?) {
        let config = s.config
        var row = Results.VMStats(name: config.name, engine: config.engine.rawValue, cpus: config.cpus, memory: config.memoryBytes ?? 0)
        row.pid = s.pid
        row.macProblem = s.macProblem
        row.balloonMode = s.balloon == nil ? nil : s.balloonMode
        var usage: ProcessUsage?
        if let pid = s.pid {
            if let now = MacResources.usage(of: pid) {
                if let before, seconds > 0 { row.cpuPercent = max(0, now.cpuSeconds - before.cpuSeconds) / seconds * 100 }
                usage = now
            } else {
                row.macProblem = "cannot read process \(pid) (it ended, or belongs to another user)"
            }
        }
        if let disk = config.disk, let image = MacResources.diskImage(disk) {
            row.diskAllocated = image.allocated
            row.diskSize = image.size
        }
        return (row, usage)
    }

    /// The guest's own use. Without the balloon's answer the memory it holds cannot be told apart from the guest's, so
    /// it counts as used, as the guest reports it.
    static func guestUsed(_ g: Results.GuestStats, balloon: UInt64?, configured: UInt64) -> UInt64 {
        guard let balloon, configured > 0 else { return g.memoryTotal - min(g.memoryAvailable, g.memoryTotal) }
        return BalloonPolicy.used(total: g.memoryTotal, available: g.memoryAvailable, actual: balloon, configured: configured)
    }

    /// `body`'s answer, or nil after `limit` while `body` carries on unwatched.
    static func answer<T: Sendable>(within limit: Duration, _ body: @escaping @Sendable () async -> T?) async -> T? {
        await withCheckedContinuation { c in
            let once = ResumeOnce(c)
            Task { once.resume(await body()) }
            Task { try? await Task.sleep(for: limit); once.resume(nil) }
        }
    }
}

/// Resumes a continuation with the first value given; later ones are dropped.
private final class ResumeOnce<T: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<T, Never>?
    init(_ continuation: CheckedContinuation<T, Never>) { self.continuation = continuation }

    func resume(_ value: T) {
        let c = lock.withLock { () -> CheckedContinuation<T, Never>? in
            defer { continuation = nil }
            return continuation
        }
        c?.resume(returning: value)
    }
}
