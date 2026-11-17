import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI
@testable import glov

private let gib: UInt64 = 1 << 30
private let calm = MacMemory(physical: 32 * gib, pressure: "normal", swapUsed: 0, swapTotal: 0)
private let squeezed = MacMemory(physical: 32 * gib, pressure: "critical", swapUsed: 8 * gib + gib / 4, swapTotal: 9 * gib)

@Suite struct StatsTests {
    // MARK: memory against the configured size

    @Test func memoryCountedTwiceIsScaledBackToTheConfiguredSize() {
        // Seen on a 12 GiB QEMU guest: top showed 13.2 GiB resident and 10.1 GiB compressed.
        let m = MemoryOnMac(resident: 13_200_000_000, compressed: 10_100_000_000, configured: 12 * gib)
        #expect(m.resident + m.compressed <= 12 * gib && m.resident + m.compressed > 12 * gib - 1024)
        #expect(m.resident > m.compressed)  // the split is kept
        let small = MemoryOnMac(resident: 2 * gib, compressed: gib, configured: 8 * gib)
        #expect(small.resident == 2 * gib && small.compressed == gib)
    }

    // MARK: the balloon

    @Test func theBalloonsMemoryIsNotCountedAsTheGuestsOwnUse() {
        // Seen on win11 (-m 12G, auto at 4.7G): the guest reported 9.8G used and 2.1G available.
        let g = Results.GuestStats(memoryTotal: 12 * gib - gib / 10, memoryAvailable: 2 * gib + gib / 10, diskPath: "C:\\", diskTotal: 0, diskUsed: 0, diskAvailable: 0)
        let used = StatsCollector.guestUsed(g, balloon: 4 * gib + 7 * gib / 10, configured: 12 * gib)
        #expect(used > 2 * gib && used < 3 * gib, "\(Double(used) / Double(gib))")
        #expect(StatsCollector.guestUsed(g, balloon: nil, configured: 12 * gib) == g.memoryTotal - g.memoryAvailable, "unknown: as the guest says")
        #expect(StatsCollector.guestUsed(g, balloon: 12 * gib, configured: 12 * gib) == g.memoryTotal - g.memoryAvailable)
    }

    @Test(.timeLimit(.minutes(1))) func statsShowTheBalloonAndTheGuestsOwnUse() async {
        let config = VMConfig(name: "win", directory: "/vm/win", memory: "12G", firmwareCode: "/fw", balloon: true)
        let guest = Results.GuestStats(memoryTotal: 12 * gib, memoryAvailable: 2 * gib, diskPath: "C:\\", diskTotal: 0, diskUsed: 0, diskAvailable: 0)
        var subject = StatsCollector.Subject(config: config) { (guest, nil) }
        subject.balloon = { 4 * gib }
        subject.balloonMode = "auto"
        let plain = StatsCollector.Subject(config: VMConfig(name: "vz", directory: "/vm/vz", memory: "4G", firmwareCode: "/fw")) { (guest, nil) }
        let stats = await StatsCollector.collect([subject, plain], mac: calm, interval: .milliseconds(10))
        let win = stats.vms[0], vz = stats.vms[1]
        #expect(win.balloonMemory == 4 * gib && win.balloonMode == "auto" && win.guestMemoryUsed == 2 * gib)
        #expect(vz.balloonMemory == nil && vz.balloonMode == nil && vz.guestMemoryUsed == 10 * gib)
        // The Mac side of a guest the balloon shrank is at most what it has: this test process stands in for its qemu.
        var small = subject
        small.pid = getpid()
        small.balloon = { 4 << 20 }
        let shrunk = await StatsCollector.collect([small], mac: calm, interval: .milliseconds(10)).vms[0]
        let resident = shrunk.memoryResident ?? 0
        #expect(resident > 0 && resident + (shrunk.memoryCompressed ?? 0) <= 4 << 20, "\(resident)")
        let table = StatsCommand.render(stats)
        #expect(table.contains("BALLOON") && table.contains("4.0G auto") && table.contains("2.0G/2.0G"), "\(table)")
    }

    @Test(.timeLimit(.minutes(1))) func aQEMUThatDoesNotAnswerLeavesTheBalloonOut() async {
        let hung: UInt64? = await StatsCollector.answer(within: .milliseconds(50)) {
            try? await Task.sleep(for: .seconds(30))
            return 1
        }
        #expect(hung == nil)
        #expect(await StatsCollector.answer(within: .seconds(5)) { UInt64(7) } == 7)
    }

    // MARK: start warning

    @Test func startingPastTwoThirdsOfTheMacWarnsNamingTheVMs() throws {
        let warning = try #require(MemoryBudget.warning(starting: ("win11", 16 * gib), running: [("ubuntu", 8 * gib)], mac: calm))
        #expect(warning.contains("starting win11 (16.0 GiB) with ubuntu (8.0 GiB) running"), "\(warning)")
        #expect(warning.contains("24.0 GiB") && warning.contains("32.0 GiB"), "\(warning)")
        #expect(MemoryBudget.warning(starting: ("win11", 16 * gib), running: [("ubuntu", 4 * gib)], mac: calm) == nil)
    }

    @Test func startingUnderMemoryPressureWarnsEvenWithinBudget() throws {
        let warning = try #require(MemoryBudget.warning(starting: ("ubuntu", 4 * gib), running: [], mac: squeezed))
        #expect(warning.contains("memory pressure (critical") && warning.contains("8.2 of 9.0 GiB"), "\(warning)")
        #expect(warning.hasPrefix("warning: starting ubuntu (4.0 GiB) while the Mac is under memory pressure"), "\(warning)")
    }

    // MARK: problems name Mac memory pressure

    @Test func anUnresponsiveGuestServiceNamesMacMemoryPressure() {
        let hung = Results.GuestServiceStatus(state: "unresponsive", problem: "no reply from the Guest Service for 20 s")
        let named = Dispatcher.withMacPressure(hung, mac: squeezed).problem ?? ""
        #expect(named.hasPrefix("no reply from the Guest Service for 20 s") && named.contains("memory pressure (critical"), "\(named)")
        #expect(Dispatcher.withMacPressure(hung, mac: calm) == hung)
        let ready = Results.GuestServiceStatus(state: "ready")
        #expect(Dispatcher.withMacPressure(ready, mac: squeezed) == ready)
    }
}

// MARK: vm.stats and glov vm stats, with this test process standing in for the VM's Mac process

extension StatsTests {
    private func sparseDisk(_ dir: String) throws -> String {
        let path = dir + "/disk.img"
        FileManager.default.createFile(atPath: path, contents: Data(repeating: 1, count: 1 << 20))
        let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
        try h.truncate(atOffset: 64 << 30)
        try h.close()
        return path
    }

    @Test(.timeLimit(.minutes(1))) func statsShowTheMacAndTheGuestSideOfARunningVM() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let f = try await GuestServiceFixture(running: true, pid: getpid(), disk: try sparseDisk(dir))
        defer { f.cleanup() }
        let stats = try await f.client.call(Method.vmStats, Params.Stats(), as: Results.Stats.self)
        #expect(stats.mac.physical == ProcessInfo.processInfo.physicalMemory)
        let vm = try #require(stats.vms.first)
        #expect(stats.vms.count == 1 && vm.name == "win11" && vm.engine == "qemu" && vm.cpus == 2 && vm.memory == 4 * gib)
        #expect(vm.pid == getpid() && vm.cpuPercent != nil && vm.macProblem == nil)
        let resident = try #require(vm.memoryResident), compressed = try #require(vm.memoryCompressed)
        #expect(resident > 0 && resident + compressed <= vm.memory)
        #expect(vm.diskSize == 64 * gib && (vm.diskAllocated ?? 0) >= 1 << 20 && (vm.diskAllocated ?? 0) < 8 << 20)
        #expect(vm.guest?.memoryTotal == 8 * gib && vm.guest?.diskUsed == 40 * gib && vm.guest?.cpuBusy == 12.5)
        #expect(vm.guestProblem == nil)
        let one = try await f.client.call(Method.vmStats, Params.Stats(vm: "win"), as: Results.Stats.self)
        #expect(one.vms.map(\.name) == ["win11"])
    }

    @Test(.timeLimit(.minutes(1))) func aGuestServiceThatIsNotReadyLeavesTheMacSideAndSaysWhy() async throws {
        let f = try await GuestServiceFixture(running: true, pid: getpid())
        defer { f.cleanup() }
        f.fake.stop()
        await eventually { (try? await f.client.call(Method.vmStatus, Params.VM(vm: "win11"), as: Results.VMInfo.self))?.agent?.state == "connecting" }
        let vm = try #require(try await f.client.call(Method.vmStats, Params.Stats(), as: Results.Stats.self).vms.first)
        #expect(vm.guest == nil && vm.guestProblem?.contains("connecting") == true, "\(vm.guestProblem ?? "")")
        #expect(vm.memoryResident != nil && vm.cpuPercent != nil)
    }

    @Test(.timeLimit(.minutes(1))) func aGuestServiceWithoutStatsAsksForAnUpdate() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        f.fake.supportsStats = false
        let vm = try #require(try await f.client.call(Method.vmStats, Params.Stats(), as: Results.Stats.self).vms.first)
        #expect(vm.guest == nil && vm.guestProblem?.contains("glov vm guest update win11") == true, "\(vm.guestProblem ?? "")")
        #expect(vm.pid == nil && vm.macProblem != nil)
    }

    @Test(.timeLimit(.minutes(1))) func statsOfAStoppedVMFail() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let error = await f.failure(Method.vmStats, try JSONValue(encoding: Params.Stats(vm: "win11")))
        #expect(error?.message.contains("not running") == true, "\(error?.message ?? "")")
        #expect(try await f.client.call(Method.vmStats, Params.Stats(), as: Results.Stats.self).vms.isEmpty)
    }

    @Test(.timeLimit(.minutes(1))) func glovStatsPrintsATableOrJSON() async throws {
        let f = try await GuestServiceFixture(running: true, pid: getpid())
        defer { f.cleanup() }
        let table = try await runglov(["vm", "stats"], home: f.home)
        #expect(table.status == 0, "\(table.err)")
        let lines = table.out.split(separator: "\n")
        #expect(lines.first?.hasPrefix("Mac: memory pressure ") == true, "\(table.out)")
        let row = try #require(lines.first { $0.hasPrefix("win11") })
        #expect(row.contains("4.0G") && row.contains("6.0G") && row.contains("40.0G/200.0G"), "\(table.out)")
        let json = try await runglov(["vm", "stats", "--json"], home: f.home)
        #expect(json.status == 0, "\(json.err)")
        let decoded = try JSONDecoder().decode(Results.Stats.self, from: Data(json.out.utf8))
        #expect(decoded.vms.first?.guest?.memoryAvailable == 6 * gib)

        f.fake.supportsStats = false
        let old = try await runglov(["vm", "stats", "win11"], home: f.home)
        #expect(old.out.contains("guest: unavailable (") && old.out.contains("glov vm guest update"), "\(old.out)")

        f.fake.statsError = "boom\u{1B}]0;title\u{07}\u{1B}[2J"
        let hostile = try await runglov(["vm", "stats", "win11"], home: f.home)
        #expect(hostile.out.contains("boom") && !hostile.out.contains("\u{1B}") && !hostile.out.contains("\u{07}"), "\(hostile.out)")
    }
}
