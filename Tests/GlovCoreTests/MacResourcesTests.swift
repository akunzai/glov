import Foundation
import Testing

@testable import GlovCore

@Suite struct MacResourcesTests {
    @Test func macMemoryReportsPhysicalMemoryPressureAndSwap() {
        let mac = MacResources.memory()
        #expect(mac.physical == ProcessInfo.processInfo.physicalMemory)
        #expect(["normal", "warning", "critical"].contains(mac.pressure))
        #expect(mac.swapUsed <= mac.swapTotal)
    }

    @Test func aProcessReportsItsCPUTimeAndResidentMemory() async throws {
        let before = try #require(MacResources.usage(of: getpid()))
        // Burned off the cooperative pool, so the other tests keep running meanwhile.
        let x = await Blocking.run {
            var x = 0.0
            for i in 0..<5_000_000 { x += sin(Double(i)) }
            return x
        }
        #expect(x.isFinite)
        let after = try #require(MacResources.usage(of: getpid()))
        #expect(after.cpuSeconds > before.cpuSeconds)
        #expect(after.resident > 1 << 20)
        #expect(MacResources.usage(of: 999_999) == nil)
    }

    @Test func memoryGivenBackAsReusableIsNotResident() throws {
        // What glov-qemu does with a balloon's pages. 1 GiB, so other tests' allocations meanwhile cannot hide the drop.
        let size = 1 << 30
        let p = try #require(mmap(nil, size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0))
        defer { munmap(p, size) }
        memset(p, 1, size)
        let touched = try #require(MacResources.usage(of: getpid())).resident
        #expect(madvise(p, size, MADV_FREE_REUSABLE) == 0)
        let freed = try #require(MacResources.usage(of: getpid())).resident
        #expect(touched > freed && touched - freed > UInt64(size / 2), "\(touched) → \(freed)")
    }

    @Test func theProcessHoldingAFileIsFoundAmongProcessesOfOneExecutable() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let disk = dir + "/disk.img"
        FileManager.default.createFile(atPath: disk, contents: Data(count: 16))
        let other = Process(), holder = Process()
        for p in [other, holder] { p.executableURL = URL(fileURLWithPath: "/bin/sleep"); p.arguments = ["30"] }
        holder.standardInput = try FileHandle(forReadingFrom: URL(fileURLWithPath: disk))
        try other.run()
        try holder.run()
        defer { other.terminate(); holder.terminate() }
        #expect(MacResources.process(holding: disk, executable: "/bin/sleep") == holder.processIdentifier)
        #expect(MacResources.process(holding: dir + "/missing", executable: "/bin/sleep") == nil)
        #expect(MacResources.process(holding: disk, executable: "/bin/cat") == nil)
    }

    @Test func aRawImageIsItsAllocatedBlocksOfItsLogicalSize() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let path = dir + "/disk.img"
        FileManager.default.createFile(atPath: path, contents: Data(repeating: 1, count: 1 << 20))
        let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
        try h.truncate(atOffset: 8 << 30)
        try h.close()
        let image = try #require(MacResources.diskImage(path))
        #expect(image.size == 8 << 30)
        #expect(image.allocated >= 1 << 20 && image.allocated < 4 << 20)
        #expect(MacResources.diskImage(dir + "/missing") == nil)
    }

    @Test func aQcow2ImageIsItsAllocatedBlocksOfItsVirtualSize() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let path = dir + "/disk.qcow2"
        // Header: magic "QFI\xfb", version 3, ..., virtual size (big-endian) at offset 24.
        var header = Data([0x51, 0x46, 0x49, 0xFB, 0, 0, 0, 3]) + Data(count: 16)
        withUnsafeBytes(of: UInt64(200 << 30).bigEndian) { header.append(contentsOf: $0) }
        header += Data(count: 65536 - header.count)
        FileManager.default.createFile(atPath: path, contents: header)
        let image = try #require(MacResources.diskImage(path))
        #expect(image.size == 200 << 30)
        #expect(image.allocated >= 65536 && image.allocated < 1 << 20)
    }
}
