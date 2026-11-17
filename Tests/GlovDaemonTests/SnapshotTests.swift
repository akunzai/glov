import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI

private let qemuImg = try? VirtualMachine.find("qemu-img")

@Suite struct DaemonSnapshotTests {
    private func call(_ d: Dispatcher, _ method: String, _ params: JSONValue) async -> RPCResponse {
        await d.handle(RPCRequest(id: 1, method: method, params: params))
    }

    @Test(.enabled(if: qemuImg != nil, "qemu-img is required"))
    func stoppedVMSnapshotsThroughTheDispatcher() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let disk = home.path + "/d.qcow2"
        let img = try #require(qemuImg)
        let create = Process()
        create.executableURL = URL(fileURLWithPath: img)
        create.arguments = ["create", "-f", "qcow2", disk, "1M"]
        create.standardOutput = FileHandle.nullDevice
        let (exited, exit) = AsyncStream<Void>.makeStream()
        create.terminationHandler = { _ in exit.yield(); exit.finish() }  // not waitUntilExit, see DaemonProcess.crash
        try create.run()
        for await _ in exited {}
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        try home.create(VMConfig(name: "win11", directory: home.directory(of: "win11"), disk: disk, firmwareCode: home.path + "/code.fd", tpm: false))
        let d = Dispatcher(home: home)

        func list() async throws -> [SnapshotInfo] {
            let r = await call(d, Method.snapshotList, ["vm": "win"])
            return try #require(r.result).decode(Results.SnapshotList.self).snapshots
        }
        #expect(try await list().isEmpty)

        let created = await call(d, Method.snapshotCreate, ["vm": "win", "name": "before-build"])
        #expect(created.error == nil)
        #expect(try await list().map(\.name) == ["before-build"])
        #expect(try await list().first?.live == false)

        let dup = await call(d, Method.snapshotCreate, ["vm": "win11", "name": "before-build"])
        #expect(dup.error?.message.contains("already exists") == true)
        let bad = await call(d, Method.snapshotCreate, ["vm": "win11", "name": "no/slash"])
        #expect(bad.error?.message.contains("invalid snapshot name") == true)
        let missing = await call(d, Method.snapshotCreate, ["vm": "win11"])
        #expect(missing.error?.code == RPCError.invalidParams)
        let unknown = await call(d, Method.snapshotRevert, ["vm": "win11", "name": "nope"])
        #expect(unknown.error?.message.contains("no snapshot") == true)

        let reverted = await call(d, Method.snapshotRevert, ["vm": "win11", "name": "before-build"])
        #expect(try #require(reverted.result).decode(Results.VMInfo.self).state == "stopped")

        #expect(await call(d, Method.snapshotDelete, ["vm": "win11", "name": "before-build"]).error == nil)
        #expect(try await list().isEmpty)
    }

    @Test func aVMWithoutADiskHasNothingToSnapshot() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let error = await f.failure(Method.snapshotCreate, ["vm": "win11", "name": "x"])
        #expect(error?.message.contains("no system disk") == true, "\(error?.message ?? "no error")")
    }

    @Test func snapshotsAreAllowedWhenAgentControlIsOff() async throws {
        let f = try await GuestServiceFixture(agentControl: false)
        defer { f.cleanup() }
        let error = await f.failure(Method.snapshotList, ["vm": "win11"])
        #expect(error?.message.contains("agent control") != true, "\(error?.message ?? "")")
    }

    @Test func restoringTheGuestReopensTheSessionLosesJobsAndSetsTheClock() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let config = VMConfig(name: "r", directory: dir, firmwareCode: "/fw", tpm: false)
        let fake = try FakeGuestService(socketPath: config.guestSocketPath)
        defer { fake.stop() }
        fake.script = { _ in [.hold] }
        let agent = VMGuestService(config: config)
        await agent.start()
        await eventually { await agent.isReady }
        let job = try await agent.execStart(Params.Exec(vm: "r", argv: ["sleep"])).jobId
        #expect(fake.accepted == 1)

        let before = Date().timeIntervalSince1970 * 1000
        await agent.guestRestored()
        await eventually("agent never became ready again") { await agent.isReady && fake.accepted == 2 }
        await eventually("no time.sync") { !fake.requests("time.sync").isEmpty }
        let sent = fake.requests("time.sync")
        #expect(sent.count == 1)
        #expect(Double(sent[0]["unixMillis"]?.intValue ?? 0) >= before)
        #expect(try await agent.execRead(Params.ExecRead(vm: "r", jobId: job)).lost)

        // Only a restore sets the clock: an ordinary reconnect does not.
        fake.dropConnection()
        await eventually { fake.accepted == 3 }
        try await Task.sleep(for: .milliseconds(500))
        #expect(fake.requests("time.sync").count == 1)
        await agent.stop()
    }

    /// A QEMU guest's clock stops while the Mac sleeps, and Windows Time did not catch up for hours.
    @Test func hostWakeSetsTheClockOfAReadyGuest() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let config = VMConfig(name: "w", directory: dir, firmwareCode: "/fw", tpm: false)
        let fake = try FakeGuestService(socketPath: config.guestSocketPath)
        defer { fake.stop() }
        let agent = VMGuestService(config: config)
        await agent.start()
        await eventually { await agent.isReady }
        #expect(fake.requests("time.sync").isEmpty)
        let before = (Date().timeIntervalSince1970 * 1000).rounded(.down)  // time.sync sends whole milliseconds
        await agent.hostWoke()
        await eventually("no time.sync") { !fake.requests("time.sync").isEmpty }
        #expect(Double(fake.requests("time.sync")[0]["unixMillis"]?.intValue ?? 0) >= before)
        // The wake is consumed: the next ordinary reconnect does not set the clock again.
        fake.dropConnection()
        await eventually { fake.accepted == 2 }
        try await Task.sleep(for: .milliseconds(500))
        #expect(fake.requests("time.sync").count == 1)
        await agent.stop()
    }

    @Test func hostWakeBeforeTheGuestServiceAnswersSetsTheClockOnceItDoes() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let config = VMConfig(name: "w", directory: dir, firmwareCode: "/fw", tpm: false)
        let agent = VMGuestService(config: config)
        await agent.start()
        await agent.hostWoke()
        let fake = try FakeGuestService(socketPath: config.guestSocketPath)
        defer { fake.stop() }
        await eventually("agent never became ready") { await agent.isReady }
        await eventually("no time.sync") { !fake.requests("time.sync").isEmpty }
        #expect(fake.requests("time.sync").count == 1)
        await agent.stop()
    }

    /// A wake while glovd was down went unseen, so a QEMU guest the next daemon reattaches to gets its clock set.
    @Test(.enabled(if: (try? VirtualMachine.find("qemu-system-aarch64")) != nil, "qemu-system-aarch64 is required"))
    func aReattachedGuestHasItsClockSet() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        try home.create(VMConfig(name: "w", directory: home.directory(of: "w"), firmwareCode: home.path + "/code.fd", tpm: false))
        let config = try home.loadConfig("w")
        // A process named qemu-system-aarch64 that answers QMP stands in for the QEMU the old daemon left running.
        let qemu = try await FakeQEMU(dir: home.path, qmpSocket: config.qmpSocketPath)
        defer { qemu.stop() }
        try "\(qemu.pid)\n".write(toFile: config.pidFilePath, atomically: true, encoding: .utf8)
        let fake = try FakeGuestService(socketPath: config.guestSocketPath)
        defer { fake.stop() }

        let dispatcher = Dispatcher(home: home)
        #expect(await dispatcher.attachRunning() == ["w"])
        let agent = try #require(await dispatcher.guestServiceForTesting("w"))
        await eventually("agent never became ready") { await agent.isReady }
        await eventually("no time.sync") { !fake.requests("time.sync").isEmpty }
        #expect(fake.requests("time.sync").count == 1)
        await dispatcher.stopGuestServiceForTesting("w")
    }
}

/// An ad-hoc signed copy of `sleep` named qemu-system-aarch64 (so `liveQEMUPid` accepts it), plus a QMP socket that
/// greets and answers every command with `{}`.
private final class FakeQEMU: @unchecked Sendable {
    let process = Process()
    var pid: Int32 { process.processIdentifier }
    private let listener: Int32

    init(dir: String, qmpSocket: String) async throws {
        let exe = dir + "/qemu-system-aarch64"
        try FileManager.default.copyItem(atPath: "/bin/sleep", toPath: exe)
        // macOS kills a copied platform binary on launch; an ad-hoc signature makes it an ordinary one.
        let sign = Process()
        sign.executableURL = URL(fileURLWithPath: "/usr/bin/codesign")
        sign.arguments = ["-f", "-s", "-", exe]
        try await runToExit(sign)
        process.executableURL = URL(fileURLWithPath: exe)
        process.arguments = ["60"]
        try process.run()

        var addr = try #require(UnixSocket.address(qmpSocket))
        listener = socket(AF_UNIX, SOCK_STREAM, 0)
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard rc == 0, listen(listener, 1) == 0 else { throw CocoaError(.fileWriteUnknown) }
        Thread.detachNewThread { [listener] in
            let fd = accept(listener, nil, nil)
            guard fd >= 0 else { return }
            defer { close(fd) }
            func send(_ json: String) { _ = UnixSocket.write(fd: fd, Data((json + "\n").utf8)) }
            send(#"{"QMP":{"version":{"qemu":{"major":11,"minor":1,"micro":2}},"capabilities":["oob"]}}"#)
            var buf = [UInt8](repeating: 0, count: 65536)
            var acc = Data()
            while true {
                let n = read(fd, &buf, buf.count)
                if n <= 0 { return }
                acc.append(contentsOf: buf[..<n])
                while let nl = acc.firstIndex(of: 10) {
                    let line = Data(acc[acc.startIndex..<nl])
                    acc.removeSubrange(acc.startIndex...nl)
                    if let msg = try? JSONDecoder().decode(JSONValue.self, from: line), case .int(let id)? = msg["id"] {
                        send(#"{"return":{},"id":\#(id)}"#)
                    }
                }
            }
        }
    }

    func stop() {
        close(listener)
        process.terminate()
        process.waitUntilExit()
    }
}
