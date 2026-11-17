import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct StatsCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "stats",
        abstract: "Show running VMs' CPU, memory and disk use from the Mac's side and the guest's, and the Mac's memory pressure.",
        discussion: """
            CPU is the VM's Mac process over a 1 s sample (100% is one Mac core) of its vCPUs. Resident and compressed are \
            the guest memory in Mac RAM and compressed or swapped out, scaled to at most the configured size (or the balloon's amount), because \
            macOS counts HVF-mapped guest memory twice. Balloon is what a QEMU guest with a balloon device has now and \
            its 'glov vm balloon' mode. Disk is the image's blocks allocated on the Mac of the size the \
            guest sees. The guest columns come from its Guest Service: memory used (without what the balloon holds) and available, the system volume's \
            used and total size, and the load average (Linux) or CPU busy share (Windows).
            """)
    @Argument(help: "VM name or unique prefix (default: every running VM).") var vm: String?
    @Flag(help: "Refresh every few seconds until interrupted.") var watch = false
    @Flag(help: "Print JSON (with --watch, one object per line).") var json = false

    func run() async throws {
        try await report {
            let client = daemonClient()
            repeat {
                let stats = try await client.call(Method.vmStats, Params.Stats(vm: vm), as: Results.Stats.self)
                if json {
                    let encoder = JSONEncoder()
                    encoder.outputFormatting = watch ? [.sortedKeys] : [.prettyPrinted, .sortedKeys]
                    print(String(decoding: try encoder.encode(stats), as: UTF8.self))
                } else {
                    if watch { print("\u{1B}[H\u{1B}[2J", terminator: "") }
                    print(Self.render(stats), terminator: "")
                }
                fflush(stdout)
                if watch { try await Task.sleep(for: .seconds(2)) }  // plus the call's own 1 s sample
            } while watch
        }
    }

    static func render(_ stats: Results.Stats) -> String {
        let mac = stats.mac
        var out = "Mac: memory pressure \(mac.pressure), swap \(size(mac.swapUsed)) of \(size(mac.swapTotal)) used, \(size(mac.physical)) physical\n"
        if stats.vms.isEmpty { return out + "no running VMs\n" }
        let header = ["VM", "CPU", "MEMORY", "BALLOON", "RESIDENT", "COMPRESSED", "DISK ALLOC/SIZE", "GUEST MEM USED/AVAIL", "GUEST DISK USED/SIZE", "LOAD"]
        var rows = [header]
        var notes: [String: [String]] = [:]
        for vm in stats.vms {
            let disk = vm.diskAllocated.map { size($0) + "/" + size(vm.diskSize ?? 0) } ?? "-"
            let balloon = vm.balloonMemory.map { size($0) + (vm.balloonMode.map { " " + $0 } ?? "") } ?? "-"
            var row = [vm.name, (vm.cpuPercent.map { String(format: "%.0f%%", $0) } ?? "-") + " of \(vm.cpus)", size(vm.memory), balloon,
                       vm.memoryResident.map(size) ?? "-", vm.memoryCompressed.map(size) ?? "-", disk]
            if let g = vm.guest {
                let load = g.load.map { $0.map { String(format: "%.2f", $0) }.joined(separator: " ") } ?? String(format: "cpu %.0f%%", g.cpuBusy)
                let used = vm.guestMemoryUsed ?? g.memoryTotal - min(g.memoryAvailable, g.memoryTotal)
                row += [size(used) + "/" + size(g.memoryAvailable), size(g.diskUsed) + "/" + size(g.diskTotal), load]
            } else {
                row += ["-", "-", "-"]
            }
            rows.append(row)
            notes[vm.name] = (vm.macProblem.map { ["mac: " + printable($0)] } ?? []) + (vm.guestProblem.map { ["guest: unavailable (" + printable($0) + ")"] } ?? [])
        }
        let widths = header.indices.map { i in rows.map { $0[i].count }.max() ?? 0 }
        for row in rows {
            out += row.enumerated().map { i, cell in i == row.count - 1 ? cell : cell.padding(toLength: widths[i], withPad: " ", startingAt: 0) }
                .joined(separator: "  ") + "\n"
            for note in notes[row[0]] ?? [] { out += "  " + note + "\n" }
        }
        return out
    }

    /// A problem can quote the guest's own error text: drop control characters so it cannot drive the terminal.
    static func printable(_ text: String) -> String {
        String(String.UnicodeScalarView(text.unicodeScalars.filter { !CharacterSet.controlCharacters.contains($0) }))
    }

    private static func size(_ bytes: UInt64) -> String { String(format: "%.1fG", Double(bytes) / Double(1 << 30)) }
}
