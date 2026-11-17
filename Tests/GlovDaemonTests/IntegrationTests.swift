import Foundation
import GlovCore
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI

private let firmware: String? = {
    let found = ["qemu-system-aarch64", "swtpm"].allSatisfy { name in
        ["/opt/homebrew/bin", "/usr/local/bin"].contains { FileManager.default.isExecutableFile(atPath: $0 + "/" + name) }
            || (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":").contains {
                FileManager.default.isExecutableFile(atPath: "\($0)/\(name)")
            }
    }
    guard found else { return nil }
    return ["/opt/homebrew/share/qemu", "/usr/local/share/qemu"].map { $0 + "/edk2-aarch64-code.fd" }
        .first { FileManager.default.fileExists(atPath: $0) }
}()

@Suite struct IntegrationTests {
    private func shot(_ client: DaemonClient, _ vm: String) async throws -> (png: Data, result: Results.Screenshot) {
        let r = try await client.call(Method.screenshot, Params.Screenshot(vm: vm), as: Results.Screenshot.self)
        return (try #require(Data(base64Encoded: r.png)), r)
    }

    /// Polls until two consecutive screenshots match, i.e. the UEFI shell prompt after the startup countdown.
    private func settledScreen(_ client: DaemonClient, _ vm: String) async throws -> Data {
        var last = Data()
        var stable = 0
        for _ in 0..<60 where stable < 3 {
            try await Task.sleep(for: .seconds(1))
            guard let s = try? await shot(client, vm) else { continue }
            stable = s.png == last ? stable + 1 : 0
            last = s.png
        }
        #expect(stable == 3, "screen never settled")
        return last
    }

    @Test(.enabled(if: firmware != nil, "qemu-system-aarch64, swtpm and the EDK2 firmware are required"), .timeLimit(.minutes(5)))
    func driveAVMThroughTheDaemonAcrossClientAndDaemonRestarts() async throws {
        let code = try #require(firmware)
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try home.create(VMConfig(
            name: "itest", directory: home.directory(of: "itest"), cpus: 2, memory: "1G", firmwareCode: code, tpm: false))

        var daemon = try DaemonProcess(home: home)
        defer {
            daemon.process.terminate()
            // Only the qemu this test started.
            if let text = try? String(contentsOfFile: home.directory(of: "itest") + "/qemu.pid", encoding: .utf8), let pid = Int32(text.trimmingCharacters(in: .whitespacesAndNewlines)) {
                kill(pid, SIGKILL)
            }
        }
        try await daemon.waitUntilReady()
        var client = daemon.client

        let started = try await client.call(Method.vmStart, Params.VM(vm: "it"), as: Results.VMInfo.self)
        #expect(started.name == "itest" && started.state == "running")
        let pid = try #require(started.pid)
        let waited = try await client.call(Method.vmWait, Params.Wait(vm: "itest", timeout: 10), as: Results.VMInfo.self)
        #expect(waited.state == "running")

        // Nothing in the UEFI shell runs the Guest Service: it stays "connecting" and the tools say so.
        #expect(waited.agent?.state == "connecting")
        do {
            _ = try await client.call(Method.vmWait, ["vm": "itest", "state": "guest-ready", "timeout": .double(0.5)])
            Issue.record("guest-ready cannot hold without a Guest Service")
        } catch let e as RPCError { #expect(e.message.contains("timed out waiting for the Guest Service")) }
        do {
            _ = try await client.call(Method.execRun, Params.Exec(vm: "itest", argv: ["x"]), as: Results.ExecRun.self)
            Issue.record("exec cannot work without a Guest Service")
        } catch let e as RPCError { #expect(e.message.contains("not connected")) }

        let baseline = try await settledScreen(client, "itest")
        #expect(baseline.prefix(4) == Data([0x89, 0x50, 0x4e, 0x47]))
        let info = try await shot(client, "itest").result
        #expect((320...1280).contains(info.width) && (240...1024).contains(info.height))

        for (method, params) in [
            (Method.type, try JSONValue(encoding: Params.TypeText(vm: "itest", text: "ver"))),
            (Method.key, try JSONValue(encoding: Params.Key(vm: "itest", key: "enter"))),
            (Method.click, try JSONValue(encoding: Params.Click(vm: "itest", x: 10, y: 10))),
            (Method.move, try JSONValue(encoding: Params.Move(vm: "itest", x: 20, y: 20))),
            (Method.drag, try JSONValue(encoding: Params.Drag(vm: "itest", fromX: 20, fromY: 20, toX: 40, toY: 40))),
            (Method.scroll, try JSONValue(encoding: Params.Scroll(vm: "itest", x: 40, y: 40, dy: 1))),
        ] { _ = try await client.call(method, params) }
        try await Task.sleep(for: .seconds(1))
        #expect(try await shot(client, "itest").png != baseline, "typing `ver` did not change the screen")

        // A client going away does not stop the VM.
        client = DaemonClient(socketPath: home.socketPath)
        #expect(try await client.call(Method.vmStatus, Params.VM(vm: "itest"), as: Results.VMInfo.self).state == "running")

        // Through MCP: the screenshot comes back as image content.
        let mcp = try MCPSession(home: home)
        defer { mcp.terminate() }
        _ = try await mcp.initialize()
        let result = try await mcp.request("tools/call", ["name": "screenshot", "arguments": ["vm": "itest", "maxWidth": 640]])
        #expect(result["isError"] as? Bool != true)
        let image = try #require((result["content"] as? [[String: Any]])?.first { $0["type"] as? String == "image" })
        #expect(image["mimeType"] as? String == "image/png")
        let base64 = try #require(image["data"] as? String)
        let png = try #require(Data(base64Encoded: base64))
        #expect(png.prefix(4) == Data([0x89, 0x50, 0x4e, 0x47]))
        let clicked = try await mcp.request("tools/call", ["name": "click", "arguments": ["vm": "itest", "x": 5, "y": 5]])
        #expect(clicked["isError"] as? Bool != true)

        // `vm delete` refuses a running VM, asking the daemon, and without one, its qemu.pid.
        var deleted = try await runglov(["vm", "delete", "itest", "--yes"], home: home)
        #expect(deleted.status != 0 && deleted.err.contains("is running"), "\(deleted.err)")

        // The daemon crashes; qemu survives and the next daemon reattaches.
        await daemon.crash()
        #expect(kill(pid, 0) == 0, "qemu died with the daemon")
        deleted = try await runglov(["vm", "delete", "itest", "--yes"], home: home)
        #expect(deleted.status != 0 && deleted.err.contains("is running"), "\(deleted.err)")
        #expect(FileManager.default.fileExists(atPath: home.directory(of: "itest") + "/vm.json"))
        daemon = try DaemonProcess(home: home)
        try await daemon.waitUntilReady()
        client = daemon.client
        let reattached = try await client.call(Method.vmStatus, Params.VM(vm: "itest"), as: Results.VMInfo.self)
        #expect(reattached.state == "running" && reattached.pid == pid && reattached.agent?.state == "connecting")
        #expect(try await shot(client, "itest").png.prefix(4) == Data([0x89, 0x50, 0x4e, 0x47]))
        // Pointer input after the restart works without a prior screenshot of this daemon's own.
        _ = try await client.call(Method.click, Params.Click(vm: "itest", x: 5, y: 5), as: Results.OK.self)

        let stopped = try await client.call(Method.vmStop, Params.Stop(vm: "itest", force: true), as: Results.VMInfo.self)
        #expect(stopped.state == "stopped")
        #expect(kill(pid, 0) == -1 && errno == ESRCH, "qemu pid \(pid) still alive")
        #expect(try await client.call(Method.vmStatus, Params.VM(vm: "itest"), as: Results.VMInfo.self).state == "stopped")
    }

    /// A live revert restarts qemu into the snapshot (snapshot-load into a running guest aborts qemu), with the
    /// agent port and a NIC attached like a real VM, and the daemon keeps working afterwards.
    @Test(.enabled(if: firmware != nil && (try? VirtualMachine.find("qemu-img")) != nil, "qemu, swtpm, qemu-img and the EDK2 firmware are required"), .timeLimit(.minutes(5)))
    func liveRevertRestartsQemuIntoTheSnapshotAndReconnectsTheGuestChannel() async throws {
        let code = try #require(firmware)
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let disk = home.path + "/d.qcow2"
        _ = try await Snapshots.run(try Snapshots.qemuImgPath(), ["create", "-f", "qcow2", disk, "64M"])
        try home.create(VMConfig(
            name: "itest", directory: home.directory(of: "itest"), cpus: 2, memory: "1G", disk: disk, firmwareCode: code, tpm: true,
            sshPort: try freeTCPPort()))
        let dispatcher = Dispatcher(home: home)
        let server = try DaemonServer(home: home, dispatcher: dispatcher)
        server.start()
        defer {
            server.stop()
            if let text = try? String(contentsOfFile: home.directory(of: "itest") + "/qemu.pid", encoding: .utf8), let pid = Int32(text.trimmingCharacters(in: .whitespacesAndNewlines)) {
                kill(pid, SIGKILL)
            }
        }
        let client = DaemonClient(socketPath: home.socketPath)

        let first = try await client.call(Method.vmStart, Params.VM(vm: "itest"), as: Results.VMInfo.self)
        let baseline = try await settledScreen(client, "itest")
        _ = try await client.call(Method.snapshotCreate, Params.Snapshot(vm: "itest", name: "live1"), as: Results.SnapshotList.self)
        _ = try await client.call(Method.type, ["vm": "itest", "text": "ver"])
        _ = try await client.call(Method.key, ["vm": "itest", "key": "enter"])
        try await Task.sleep(for: .seconds(1))
        #expect(try await shot(client, "itest").png != baseline)

        let reverted = try await client.call(Method.snapshotRevert, Params.Snapshot(vm: "itest", name: "live1"), as: Results.VMInfo.self)
        #expect(reverted.state == "running" && reverted.pid != nil && reverted.pid != first.pid, "qemu must have been restarted")
        try await Task.sleep(for: .seconds(2))
        #expect(try await shot(client, "itest").png == baseline, "revert did not restore the screen")
        let listed = try await client.call(Method.snapshotList, Params.VM(vm: "itest"), as: Results.SnapshotList.self)
        #expect(listed.snapshots.map(\.name) == ["live1"] && listed.snapshots[0].live)

        do {
            _ = try await client.call(Method.snapshotRevert, ["vm": "itest", "name": "nope"])
            Issue.record("an unknown snapshot cannot be reverted")
        } catch let e as RPCError { #expect(e.message.contains("no snapshot")) }

        _ = try await client.call(Method.vmStop, Params.Stop(vm: "itest", force: true), as: Results.VMInfo.self)
        _ = try await client.call(Method.snapshotDelete, Params.Snapshot(vm: "itest", name: "live1"), as: Results.SnapshotList.self)
    }
}

private func freeTCPPort() throws -> Int {
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
