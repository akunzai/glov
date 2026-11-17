import Foundation
@testable import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

// The limit is far above what a watch needs (about 150 ms), since a loaded CI runner stalls for seconds at a time.
private let fast = InstallTiming(bootDelay: .milliseconds(50), cdKeyDelay: .milliseconds(30), timeout: .seconds(30), poll: .milliseconds(20))

/// What a watch sent and how often it was asked.
private final class Recorder: @unchecked Sendable {
    private let lock = NSLock()
    private var _steps: [WindowsInstall.Step] = []
    private var _polls = 0
    var steps: [WindowsInstall.Step] { lock.withLock { _steps } }
    var polls: Int { lock.withLock { _polls } }
    func add(_ s: WindowsInstall.Step) { lock.withLock { _steps.append(s) } }
    func poll() -> Int { lock.withLock { _polls += 1; return _polls } }
}

@Suite struct WindowsInstallTests {
    @Test func theBootLineIsTypedAfterTheDelayThenTheCDKey() {
        #expect(WindowsInstall.bootSteps(.windows) == [
            .pause(.seconds(30)), .type(#"fs0:\efi\boot\bootaa64.efi"#), .key("enter"), .pause(.milliseconds(1500)), .key("space"),
        ])
        #expect(InstallTiming.windows.timeout == .seconds(3600))
        // Every character of the boot line can be typed on a US layout.
        #expect(throws: Never.self) { for c in #"fs0:\efi\boot\bootaa64.efi"# { _ = try Input.qcodes(for: c) } }
    }

    @Test func aFirstStartTypesTheBootLineAndFinishesWhenTheGuestIsDone() async {
        let r = Recorder()
        let start = ContinuousClock.now
        let outcome = await WindowsInstall.watch(
            started: Date(), timing: fast, typeBoot: true, send: { r.add($0) }, isRunning: { true }, finished: { r.poll() >= 3 }, log: { _ in })
        #expect(outcome == .finished)
        #expect(r.steps == [.type(#"fs0:\efi\boot\bootaa64.efi"#), .key("enter"), .key("space")])
        #expect(ContinuousClock.now - start >= .milliseconds(80), "the boot delay and the CD key delay are waited out")
    }

    @Test func aReattachedInstallTypesNothing() async {
        let r = Recorder()
        let outcome = await WindowsInstall.watch(
            started: Date(), timing: fast, typeBoot: false, send: { r.add($0) }, isRunning: { true }, finished: { true }, log: { _ in })
        #expect(outcome == .finished && r.steps.isEmpty)
    }

    @Test func theLimitRunsFromTheFirstStart() async {
        let outcome = await WindowsInstall.watch(
            started: Date().addingTimeInterval(-29.9), timing: fast, typeBoot: false, send: { _ in }, isRunning: { true }, finished: { false },
            log: { _ in })
        #expect(outcome == .timedOut)
        // 60 minutes after the first start, a daemon that restarted gives up at once.
        let late = await WindowsInstall.watch(
            started: Date().addingTimeInterval(-3601), timing: .windows, typeBoot: false, send: { _ in }, isRunning: { true },
            finished: { false }, log: { _ in })
        #expect(late == .timedOut)
    }

    @Test func aVMThatStopsEndsTheWatch() async {
        let r = Recorder()
        let outcome = await WindowsInstall.watch(
            started: Date(), timing: fast, typeBoot: true, send: { r.add($0) }, isRunning: { r.poll() < 3 }, finished: { false }, log: { _ in })
        #expect(outcome == .stopped)
        #expect(r.steps == [.type(#"fs0:\efi\boot\bootaa64.efi"#)])
    }

    @Test func aTypingFailureIsLoggedAndTheWaitGoesOn() async {
        struct Boom: Error {}
        let logged = Recorder()
        let outcome = await WindowsInstall.watch(
            started: Date(), timing: fast, typeBoot: true, send: { _ in throw Boom() }, isRunning: { true }, finished: { true },
            log: { _ in logged.add(.key("logged")) })
        #expect(outcome == .finished && logged.steps.count == 1)
    }

    @Test(.timeLimit(.minutes(1))) func aCancelledWatchEndsAtOnceWithoutPolling() async {
        let r = Recorder()
        var slow = fast
        slow.poll = .seconds(30)
        slow.timeout = .seconds(600)
        let task = Task {
            await WindowsInstall.watch(
                started: Date(), timing: slow, typeBoot: false, send: { r.add($0) }, isRunning: { true }, finished: { _ = r.poll(); return false },
                log: { _ in r.add(.key("logged")) })
        }
        while r.polls == 0 { try? await Task.sleep(for: .milliseconds(5)) }
        let start = ContinuousClock.now
        task.cancel()
        #expect(await task.value == .cancelled)
        #expect(ContinuousClock.now - start < .seconds(1))
        #expect(r.polls == 1, "a cancelled watch polls no more")

        // Cancelled while typing the boot line: no "typing failed" is logged.
        let typing = Task {
            await WindowsInstall.watch(
                started: Date(), timing: .windows, typeBoot: true, send: { r.add($0) }, isRunning: { true }, finished: { false },
                log: { _ in r.add(.key("logged")) })
        }
        try? await Task.sleep(for: .milliseconds(50))
        typing.cancel()
        #expect(await typing.value == .cancelled)
        #expect(r.steps.isEmpty)
    }

    @Test func editionsHaveOneCheck() throws {
        #expect(try WindowsAssets.edition("core") == "Core" && WindowsAssets.edition("PROFESSIONAL") == "Professional")
        #expect(throws: ProvisionError.self) { try WindowsAssets.edition("Education") }
        #expect(WindowsProvisioner(name: "w", iso: "", virtio: "", guestServiceBinary: "", firmwareCode: "").diskSize == 200 << 30)
    }

    @Test(.timeLimit(.minutes(1))) func guestServiceReadyWaitsForTheInstallAndFailsWithIt() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        var config = try f.home.loadConfig("win11")
        config.install = .init(drives: [], started: Date())
        try f.home.update("win11") { [c = config] config in config = c }
        // The Guest Service answers, but setup.ps1 is not done.
        let pending = await f.failure(Method.vmWait, ["vm": "win11", "state": "guest-ready", "timeout": .double(0.3)])
        #expect(pending?.message.contains("timed out waiting for the Windows install") == true, "\(pending?.message ?? "no error")")

        // It finishes while a wait is under way: the wait returns.
        let waiting = Task { try await f.client.call(Method.vmWait, Params.Wait(vm: "win11", state: "guest-ready", timeout: 10), as: Results.VMInfo.self) }
        try await Task.sleep(for: .milliseconds(300))
        try f.home.update("win11") { [c = config.finishingInstall()] config in config = c }
        #expect(try await waiting.value.agent?.state == "ready")

        // A failed install fails the wait at once.
        config.install?.failure = "the VM stopped before the install finished"
        try f.home.update("win11") { [c = config] config in config = c }
        let start = ContinuousClock.now
        let failed = await f.failure(Method.vmWait, ["vm": "win11", "state": "guest-ready", "timeout": 30])
        #expect(failed?.message.contains("failed: the VM stopped") == true && ContinuousClock.now - start < .seconds(2))
    }

    @Test func progressNamesThePhaseFromTheStartAndTheGuestService() {
        let start = Date(timeIntervalSince1970: 0), t = InstallTiming.windows
        func phase(_ s: Double, ready: Bool = false) -> String {
            WindowsInstall.progress(started: start, now: start + s, timing: t, guestReady: ready).phase
        }
        #expect(phase(10) == "booting the installer" && phase(31) == "booting the installer")
        #expect(phase(33) == "Windows Setup" && phase(600) == "Windows Setup")
        #expect(phase(600, ready: true) == "first sign-in and setup script")
        let p = WindowsInstall.progress(started: start, now: start + 125.7, timing: t, guestReady: false)
        #expect(p.elapsedSeconds == 125 && p.limitSeconds == 3600 && !p.overdue)
        #expect(WindowsInstall.progress(started: start, now: start + 901, timing: t, guestReady: false).overdue)
        #expect(WindowsInstall.progress(started: start + 5, now: start, timing: t, guestReady: false).elapsedSeconds == 0, "a clock step back")
    }

    @Test func stateAndRefusalFollowTheInstall() {
        let started = Date()
        #expect(WindowsInstall.state(running: true, install: nil) == "running")
        #expect(WindowsInstall.state(running: false, install: nil) == "stopped")
        #expect(WindowsInstall.state(running: false, install: .init(drives: [])) == "stopped")
        #expect(WindowsInstall.state(running: true, install: .init(drives: [], started: started)) == "installing")
        #expect(WindowsInstall.state(running: false, install: .init(drives: [], started: started)) == "install-failed")
        #expect(WindowsInstall.state(running: true, install: .init(drives: [], started: started, failure: "x")) == "install-failed")

        #expect(WindowsInstall.refusal("w", nil) == nil)
        #expect(WindowsInstall.refusal("w", .init(drives: [])) == nil)
        let interrupted = WindowsInstall.refusal("w", .init(drives: [], started: started))
        #expect(interrupted?.contains("was interrupted") == true && interrupted?.contains("glov vm delete w") == true)
        #expect(WindowsInstall.refusal("w", .init(drives: [], started: started, failure: "timed out"))?.contains("failed: timed out") == true)
    }
}

private let firmware: String? = {
    guard (try? VirtualMachine.find("qemu-system-aarch64")) != nil else { return nil }
    return ["/opt/homebrew/share/qemu", "/usr/local/share/qemu"].map { $0 + "/edk2-aarch64-code.fd" }
        .first { FileManager.default.fileExists(atPath: $0) }
}()

/// The daemon side of an install under real QEMU, booting EDK2 with stand-in install drives. No Guest Service runs in the
/// UEFI shell, so the test says when the guest is done.
@Suite struct WindowsInstallQEMUTests {
    private func makeVM(_ home: GlovHome, _ name: String, code: String) throws -> VMConfig {
        let dir = home.directory(of: name)
        try FileManager.default.createDirectory(atPath: dir + "/secrets", withIntermediateDirectories: true)
        let drives = [home.path + "/win.iso", home.path + "/virtio.iso", dir + "/secrets/unattend.cdr"]
        for d in drives where !FileManager.default.fileExists(atPath: d) { FileManager.default.createFile(atPath: d, contents: Data(count: 1 << 20)) }
        FileManager.default.createFile(atPath: home.path + "/mine.img", contents: Data(count: 1 << 20))
        try home.create(VMConfig(
            name: name, directory: dir, cpus: 1, memory: "512M", firmwareCode: code, tpm: false, network: true,
            extraDrives: [.init(path: drives[0], cdrom: true, bootIndex: 1), .init(path: drives[1], cdrom: true),
                .init(path: drives[2], removable: true), .init(path: home.path + "/mine.img")],
            install: .init(drives: drives)))
        return try home.loadConfig(name)
    }

    @Test(.enabled(if: firmware != nil, "qemu-system-aarch64 and the EDK2 firmware are required"), .timeLimit(.minutes(2)))
    func anInstallFinishesUnpluggingItsDrivesOrFailsAndIsNotResumed() async throws {
        let code = try #require(firmware)
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        // Each VM has its own daemon: the one that finishes gets a limit a loaded test host cannot hit.
        var patient = fast
        patient.timeout = .seconds(60)
        let finishing = Dispatcher(home: home, installTiming: patient)
        var quick = fast
        quick.timeout = .seconds(2)
        let failing = Dispatcher(home: home, installTiming: quick)
        let done = Recorder()
        await finishing.setInstallDoneCheckForTesting { _ in done.poll() >= 10 }
        await failing.setInstallDoneCheckForTesting { _ in false }
        func call<T: Decodable>(_ method: String, _ params: JSONValue, as: T.Type) async throws -> T {
            let dispatcher = params["vm"] == "ok" ? finishing : failing
            let r = await dispatcher.handle(RPCRequest(id: 1, method: method, params: params))
            if let e = r.error { throw e }
            return try #require(r.result).decode(T.self)
        }
        func stopAll() async {
            for (d, n) in [(finishing, "ok"), (failing, "slow")] { _ = await d.handle(RPCRequest(id: 1, method: Method.vmStop, params: ["vm": .string(n), "force": true])) }
        }

        // Finishes: drives unplugged live, gone from vm.json, the secret one deleted, the user's drive kept.
        let ok = try makeVM(home, "ok", code: code)
        defer { Task { await stopAll() } }
        let started = try await call(Method.vmStart, ["vm": "ok"], as: Results.VMInfo.self)
        #expect(started.state == "installing")
        #expect(try home.loadConfig("ok").install?.started != nil)
        await finishing.installFinishedForTesting("ok")
        let after = try home.loadConfig("ok")
        #expect(after.install == nil && after.extraDrives == [.init(path: home.path + "/mine.img")])
        #expect(!FileManager.default.fileExists(atPath: ok.directory + "/secrets/unattend.cdr"))
        #expect(FileManager.default.fileExists(atPath: home.path + "/win.iso"), "shared media are never deleted")
        #expect(try await call(Method.vmStatus, ["vm": "ok"], as: Results.VMInfo.self).state == "running")
        let vm = try #require(await finishing.machineForTesting("ok"))
        guard case .array(let block) = try await vm.qmpClient().execute("query-block") else { Issue.record("query-block"); return }
        let devices = block.compactMap { b -> String? in if case .string(let d)? = b["device"] { d } else { nil } }
        #expect(!devices.contains("extra0") && !devices.contains("extra1") && !devices.contains("extra2") && devices.contains("extra3"), "\(devices)")

        // Fails: the limit passes without the guest being done; the VM keeps running for a look, and no start resumes it.
        _ = try makeVM(home, "slow", code: code)
        _ = try await call(Method.vmStart, ["vm": "slow"], as: Results.VMInfo.self)
        await failing.installFinishedForTesting("slow")
        let failed = try await call(Method.vmStatus, ["vm": "slow"], as: Results.VMInfo.self)
        #expect(failed.state == "install-failed" && failed.pid != nil)
        #expect(failed.installFailure?.contains("not done within") == true)
        #expect(!FileManager.default.fileExists(atPath: home.directory(of: "slow") + "/secrets/unattend.cdr"), "a failed install keeps no password")
        #expect(FileManager.default.fileExists(atPath: home.path + "/win.iso"))
        await #expect(throws: RPCError.self) { try await call(Method.vmWait, ["vm": "slow", "state": "guest-ready", "timeout": 5], as: Results.VMInfo.self) }
        _ = try await call(Method.vmStop, ["vm": "slow", "force": true], as: Results.VMInfo.self)
        do {
            _ = try await call(Method.vmStart, ["vm": "slow"], as: Results.VMInfo.self)
            Issue.record("a failed install must not start again")
        } catch let e as RPCError { #expect(e.message.contains("not resumed")) }
        await stopAll()
    }
}

@Suite struct VMInfoCodingTests {
    /// `VMInfo` spells out its `CodingKeys`, so a field left out of them is silently dropped on the way to the CLI.
    @Test func everyFieldSurvivesTheWire() throws {
        var info = Results.VMInfo(name: "w", state: "installing", pid: 1, agent: .init(state: "ready"), engine: "qemu", installFailure: "x")
        info.install = .init(phase: "Windows Setup", elapsedSeconds: 1, limitSeconds: 2)
        info.warning = "low memory"
        let unset = Mirror(reflecting: info).children.filter { "\($0.value)" == "nil" }.map { $0.label ?? "?" }
        #expect(unset.isEmpty, "set the new fields here too: \(unset)")
        #expect(try JSONDecoder().decode(Results.VMInfo.self, from: JSONEncoder().encode(info)) == info)
    }
}
