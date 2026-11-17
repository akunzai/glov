import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// `vm.reboot` through the daemon: a Windows (QEMU) guest restarts through the Guest Service's `power.reboot`, a Linux
/// (VZ) guest is stopped and started by the daemon; both return once a Guest Service from a new boot is ready.
@Suite struct RebootTests {
    private func reboot(_ f: GuestServiceFixture, timeout: Double? = 10, force: Bool? = nil) async throws -> Results.VMInfo {
        try await f.client.call(Method.vmReboot, Params.Reboot(vm: "win11", timeout: timeout, force: force), as: Results.VMInfo.self)
    }

    private func failure(_ f: GuestServiceFixture, vm: String = "win11", timeout: Double? = 10, force: Bool? = nil) async throws -> RPCError? {
        await f.failure(Method.vmReboot, try JSONValue(encoding: Params.Reboot(vm: vm, timeout: timeout, force: force)))
    }

    @Test(.timeLimit(.minutes(1)))
    func aWindowsRebootReturnsOnlyOnceTheNewBootIsReady() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        // The old boot keeps answering for a while after power.reboot: that must not count as the new one.
        f.fake.rebootsAfterAnswering(to: "boot-2", goingDown: 0.5)
        let info = try await reboot(f)
        #expect(f.fake.requests("power.reboot").count == 1)
        #expect(info.name == "win11" && info.state == "running")
        #expect(info.agent?.state == "ready" && info.agent?.bootId == "boot-2")
        // The same machine (QEMU process) keeps running.
        #expect(await f.dispatcher.guestServiceForTesting("win11") != nil)
    }

    @Test(.timeLimit(.minutes(1)))
    func aTimeoutReportsTheStatesAndSuggestsAScreenshotBeforeForcing() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        // power.reboot is answered but the guest never comes back with a new boot.
        let error = try #require(try await failure(f, timeout: 0.5))
        #expect(error.message.contains("running") && error.message.contains("Guest Service ready"), "\(error.message)")
        #expect(error.message.contains("glov vm screenshot win11") && error.message.contains("glov vm stop --force win11"), "\(error.message)")
        let screenshot = try #require(error.message.range(of: "glov vm screenshot"))
        let stop = try #require(error.message.range(of: "glov vm stop --force"))
        #expect(screenshot.lowerBound < stop.lowerBound)
        #expect(f.fake.requests("power.reboot").count == 1)
        // Nothing was forced: the VM still runs.
        let status: Results.VMInfo = try await f.client.call(Method.vmStatus, Params.VM(vm: "win11"))
        #expect(status.state == "running")
    }

    @Test(.timeLimit(.minutes(1)))
    func aWindowsGuestWhoseGuestServiceIsNotReadyIsNotRebooted() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        f.fake.answering = false
        f.fake.dropConnection()
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        await eventually { await a.status().state == "connecting" }
        let error = try #require(try await failure(f))
        #expect(error.message.contains("not ready"), "\(error.message)")
        #expect(error.message.contains("glov vm stop win11") && error.message.contains("--force") && error.message.contains("glov vm start win11"), "\(error.message)")
        #expect(f.fake.requests("power.reboot").isEmpty)
        let status: Results.VMInfo = try await f.client.call(Method.vmStatus, Params.VM(vm: "win11"))
        #expect(status.state == "running")
    }

    @Test(.timeLimit(.minutes(1)))
    func runningJobsBlockARebootUnlessForced() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        f.fake.script = { argv in argv == ["hold"] ? [.hold] : [.exit(0)] }
        let started: Results.JobStarted = try await f.client.call(Method.execStart, Params.Exec(vm: "win11", argv: ["hold"]))
        let error = try #require(try await failure(f))
        #expect(error.message.contains("job \(started.jobId)") && error.message.contains("--force"), "\(error.message)")
        #expect(f.fake.requests("power.reboot").isEmpty)

        f.fake.rebootsAfterAnswering(to: "boot-2")
        let info = try await reboot(f, force: true)
        #expect(info.agent?.bootId == "boot-2")
        let read: Results.ExecRead = try await f.client.call(Method.execRead, Params.ExecRead(vm: "win11", jobId: started.jobId))
        #expect(read.lost)
    }

    @Test(.timeLimit(.minutes(1)))
    func aShareSyncBlocksARebootAndForceCancelsIt() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        let host = f.home.path + "/proj"
        try FileManager.default.createDirectory(atPath: host, withIntermediateDirectories: true)
        try f.home.update("win11") { config in config = try config.adding(.init(name: "proj", hostPath: host, guestPath: "C:\\work\\proj")) }
        f.fake.holdManifest = true
        let sync = Task { try await f.client.call(Method.shareSync, Params.ShareSync(vm: "win11", share: "proj"), as: Results.ShareSync.self) }
        await eventually { !f.fake.requests("sync.manifest").isEmpty }

        let error = try #require(try await failure(f))
        #expect(error.message.contains("share sync proj") && error.message.contains("--force"), "\(error.message)")
        #expect(f.fake.requests("power.reboot").isEmpty)

        f.fake.rebootsAfterAnswering(to: "boot-2")
        let info = try await reboot(f, force: true)
        #expect(info.agent?.bootId == "boot-2")
        await #expect(throws: (any Error).self) { _ = try await sync.value }
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        #expect(await a.syncing.isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func aLinuxGuestIsStoppedAndStartedByTheDaemon() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        f.fake.os = "linux"
        let vz = { var c = f.config; c.engine = .vz; return c }()
        let old = FixtureMachine(config: vz)
        await f.dispatcher.attachMachineForTesting(old)
        let starts = Counter()
        await f.dispatcher.setRestartForTesting { [config = f.config, dispatcher = f.dispatcher, fake = f.fake] name in
            #expect(name == "win11")
            starts.add()
            fake.bootId = "boot-2"
            await dispatcher.attachMachineForTesting(FixtureMachine(config: vz))
            await dispatcher.startGuestService(config)
        }
        let info = try await reboot(f)
        #expect(starts.value == 1)
        #expect(await old.isRunning == false)
        #expect(f.fake.requests("power.reboot").isEmpty)
        #expect(info.state == "running" && info.agent?.state == "ready" && info.agent?.bootId == "boot-2")
    }

    @Test(.timeLimit(.minutes(1)))
    func aStoppedVMIsNotRebooted() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let error = try #require(try await failure(f))
        #expect(error.message.contains("not running"), "\(error.message)")
        #expect(f.fake.requests("power.reboot").isEmpty)
    }
}

/// A count shared with a `@Sendable` closure.
private final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0
    var value: Int { lock.withLock { count } }
    func add() { lock.withLock { count += 1 } }
}
