import Foundation
import Testing

@testable import GlovCore

private let qemuShare: String? = {
    guard (try? VirtualMachine.find("qemu-system-aarch64")) != nil, (try? VirtualMachine.find("swtpm")) != nil else { return nil }
    let code = ["/opt/homebrew/share/qemu", "/usr/local/share/qemu"].map { $0 + "/edk2-aarch64-code.fd" }
    return code.first { FileManager.default.fileExists(atPath: $0) }
}()

@Suite struct QEMUIntegrationTests {
    @Test(.enabled(if: qemuShare != nil, "qemu-system-aarch64, swtpm and the EDK2 firmware are required"))
    func bootsToUEFIShellTakesScreenshotTypesAndStops() async throws {
        let code = try #require(qemuShare)
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let size = try #require(FileManager.default.attributesOfItem(atPath: code)[.size] as? Int)
        let vars = dir + "/vars.fd"
        FileManager.default.createFile(atPath: vars, contents: nil)
        let h = try FileHandle(forWritingTo: URL(fileURLWithPath: vars))
        try h.truncate(atOffset: UInt64(size))
        try h.close()

        let vm = try VirtualMachine(config: VMConfig(
            name: "itest", directory: dir, cpus: 2, memory: "1G", firmwareCode: code, firmwareVars: vars, tpm: false))
        try await vm.start()
        let pid = try #require(await vm.processID)
        let console = try await vm.console()

        // The Guest Service port exists: QEMU listens on guest.sock, and nothing answers until a Guest Service runs.
        let guestSock = dir + "/guest.sock"
        let chardevs = try await vm.qmpClient().execute("query-chardev")
        guard case .array(let list) = chardevs else { Issue.record("query-chardev returned \(chardevs)"); return }
        #expect(list.contains { $0["label"] == "guest0" }, "chardev guest0 missing: \(list)")
        var st = stat()
        #expect(stat(guestSock, &st) == 0 && (st.st_mode & S_IFMT) == S_IFSOCK)
        let probe = try await Blocking.run { try UnixSocket.connect(path: guestSock) }
        close(probe)
        let channel = GuestChannel(socketPath: guestSock)
        await channel.start()
        try await Task.sleep(for: .seconds(1))
        #expect(await channel.isReady == false, "no Guest Service runs in the UEFI shell")
        await channel.stop()

        // Wait until the screen stops changing: the shell prompt, after the startup.nsh countdown.
        var last = Data()
        var stable = 0
        var baseline = Screenshot(png: Data(), size: PixelSize(width: 0, height: 0), frame: PixelSize(width: 0, height: 0))
        for _ in 0..<60 where stable < 3 {
            try await Task.sleep(for: .seconds(1))
            guard let s = try? await console.screenshot() else { continue }
            stable = (s.png == last && s.size.width > 0) ? stable + 1 : 0
            last = s.png
            baseline = s
        }
        #expect(stable == 3, "screen never settled")
        #expect(baseline.png.prefix(4) == Data([0x89, 0x50, 0x4e, 0x47]))
        #expect((320...1280).contains(baseline.size.width) && (240...1024).contains(baseline.size.height))

        try await console.type("ver")
        try await console.key("enter")
        try await Task.sleep(for: .seconds(1))
        let after = try await console.screenshot()
        #expect(after.png != baseline.png, "typing `ver` did not change the screen")

        await vm.stop(timeout: .seconds(3))
        #expect(await vm.isRunning == false)
        #expect(kill(pid, 0) == -1 && errno == ESRCH, "qemu pid \(pid) still alive")
    }

    @Test(.enabled(if: qemuShare != nil && qemuImgAvailable, "qemu-system-aarch64, swtpm, qemu-img and the EDK2 firmware are required"), .timeLimit(.minutes(5)))
    func liveAndStoppedSnapshotsCreateRevertListAndDelete() async throws {
        let code = try #require(qemuShare)
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let size = try #require(FileManager.default.attributesOfItem(atPath: code)[.size] as? Int)
        let vars = dir + "/vars.fd"
        FileManager.default.createFile(atPath: vars, contents: nil)
        let h = try FileHandle(forWritingTo: URL(fileURLWithPath: vars))
        try h.truncate(atOffset: UInt64(size))
        try h.close()
        let disk = dir + "/disk.qcow2"
        _ = try await Snapshots.run(try Snapshots.qemuImgPath(), ["create", "-f", "qcow2", disk, "64M"])

        let uuid = UUID().uuidString.lowercased()
        let config = VMConfig(
            name: "snap", directory: dir, cpus: 2, memory: "1G", disk: disk, firmwareCode: code, firmwareVars: vars,
            tpm: true, sshPort: try freePort(), macAddress: VMConfig.randomMACAddress(), uuid: uuid, diskSerial: VMConfig.randomDiskSerial())
        let vm = try VirtualMachine(config: config)
        try await vm.start()
        defer { Task { await vm.stop(timeout: .zero) } }
        #expect(try await vm.qmpClient().execute("query-uuid")["UUID"] == .string(uuid))
        let console = try await vm.console()

        func settled() async throws -> Data {
            var last = Data()
            var stable = 0
            for _ in 0..<60 where stable < 3 {
                try await Task.sleep(for: .seconds(1))
                guard let s = try? await console.screenshot() else { continue }
                stable = (s.png == last && s.size.width > 0) ? stable + 1 : 0
                last = s.png
            }
            #expect(stable == 3, "screen never settled")
            return last
        }
        let baseline = try await settled()

        // Live: save, change the screen, load, and the screen is back.
        try await vm.createSnapshot("live1")
        await #expect(throws: SnapshotError.exists("live1")) { try await vm.createSnapshot("live1") }
        // The machine it was taken on is recorded, and the same machine may load it.
        #expect(try Snapshots.savedMachine(config, name: "live1")?.cpus == 2)
        try await Snapshots.requireMachine(config, name: "live1")
        var smaller = config
        smaller.memory = "512M"
        await #expect(throws: SnapshotError.self) { try await Snapshots.requireMachine(smaller, name: "live1") }
        var listed = try await vm.listSnapshots()
        #expect(listed.map(\.name) == ["live1"] && listed[0].live && listed[0].vmStateSize > 0)

        try await console.type("ver")
        try await console.key("enter")
        try await Task.sleep(for: .seconds(1))
        let changed = try await settled()
        #expect(changed != baseline)

        // Reverting restarts QEMU into the snapshot: snapshot-load into a running guest aborts QEMU.
        try await vm.requireLiveSnapshot("live1")
        await vm.stop(timeout: .zero)
        #expect(await vm.isRunning == false)
        let vm1 = try VirtualMachine(config: config)
        try await vm1.start(loadSnapshot: "live1")
        defer { Task { await vm1.stop(timeout: .zero) } }
        let console1 = try await vm1.console()
        try await Task.sleep(for: .seconds(2))
        #expect(try await console1.screenshot().png == baseline, "revert did not restore the screen")
        #expect(try await vm1.qmpClient().execute("query-status")["status"] == "running")
        #expect(try await vm1.listSnapshots().map(\.name) == ["live1"])

        // Stopped: disk-only snapshot, its TPM state, and a live snapshot reverted from a stopped VM.
        await vm1.stop(timeout: .seconds(3))
        #expect(await vm1.isRunning == false)
        let stopped = try await Snapshots.listStopped(config)
        #expect(stopped.map(\.name) == ["live1"] && stopped[0].live)
        try await Snapshots.createStopped(config, name: "cold1")
        #expect(FileManager.default.fileExists(atPath: dir + "/snapshots/cold1/tpm"))
        #expect(try Snapshots.savedMachine(config, name: "cold1") == nil, "a disk-only snapshot has no machine to check")
        try await Snapshots.revertStopped(config, name: "cold1")
        #expect(await vm1.isRunning == false, "a disk-only revert leaves the VM stopped")

        let vm2 = try VirtualMachine(config: config)
        try await vm2.start(loadSnapshot: "live1")
        defer { Task { await vm2.stop(timeout: .zero) } }
        #expect(try await vm2.qmpClient().execute("query-status")["status"] == "running")
        let console2 = try await vm2.console()
        try await Task.sleep(for: .seconds(2))
        #expect(try await console2.screenshot().png == baseline, "starting into a live snapshot did not restore the screen")

        try await vm2.deleteSnapshot("live1")
        #expect(!FileManager.default.fileExists(atPath: dir + "/snapshots/live1"), "deleting drops the saved machine")
        listed = try await vm2.listSnapshots()
        #expect(listed.map(\.name) == ["cold1"])
        await #expect(throws: SnapshotError.needsStoppedVM("cold1")) { try await vm2.requireLiveSnapshot("cold1") }
        await #expect(throws: SnapshotError.notFound("zzz", known: ["cold1"])) { try await vm2.deleteSnapshot("zzz") }
        await vm2.stop(timeout: .seconds(3))
        try await Snapshots.deleteStopped(config, name: "cold1")
        #expect(try await Snapshots.listStopped(config).isEmpty)
    }
}

/// A TCP port nobody listens on right now, for the user-mode NIC's host forward (so virtio-net is attached as in a real VM).
private func freePort() throws -> Int {
    let fd = socket(AF_INET, SOCK_STREAM, 0)
    defer { close(fd) }
    var addr = sockaddr_in()
    addr.sin_family = sa_family_t(AF_INET)
    addr.sin_addr.s_addr = UInt32(0x7f000001).bigEndian
    let bound = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
    var len = socklen_t(MemoryLayout<sockaddr_in>.size)
    let named = withUnsafeMutablePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &len) } }
    guard bound == 0, named == 0 else { throw CocoaError(.fileWriteUnknown) }
    return Int(UInt16(bigEndian: addr.sin_port))
}
