import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// A daemon in this process over a real socket, with one VM ("win11") whose Guest Service is a `FakeGuestService`.
/// There is no qemu: the Guest Service channel is started directly, the way `vm.start` starts it.
struct GuestServiceFixture {
    let home: GlovHome
    let server: DaemonServer
    let dispatcher: Dispatcher
    let fake: FakeGuestService
    let config: VMConfig
    var client: DaemonClient { DaemonClient(socketPath: home.socketPath) }
    /// An allowlisted host directory.
    var shared: String { config.directory + "/shared" }

    /// With `running`, the VM's Mac process is `pid` (none by default) and its system disk `disk`.
    init(
        agentControl: Bool = true, agent: Bool = true, allowlist: [String]? = nil, waitForGuestService: Bool = true, guestServiceDirectory: String? = nil, running: Bool = false,
        pid: Int32? = nil, disk: String? = nil
    ) async throws {
        home = GlovHome(path: try shortTempDir())
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        try home.create(VMConfig(
            name: "win11", directory: home.directory(of: "win11"), disk: disk, firmwareCode: home.path + "/code.fd", tpm: false,
            guestService: agent, agentControl: agentControl, hostPathAllowlist: allowlist))
        config = try home.loadConfig("win11")
        fake = try FakeGuestService(socketPath: config.guestSocketPath)
        dispatcher = Dispatcher(home: home, guestServiceDirectory: guestServiceDirectory ?? home.path + "/guest-services")
        server = try DaemonServer(home: home, dispatcher: dispatcher)
        server.start()
        if running { await dispatcher.attachMachineForTesting(FixtureMachine(config: config, processID: pid)) }
        await dispatcher.startGuestService(config)
        if waitForGuestService && agent {
            _ = try await DaemonClient(socketPath: home.socketPath).call(Method.vmWait, Params.Wait(vm: "win", state: "guest-ready", timeout: 10), as: Results.VMInfo.self)
        }
    }

    func cleanup() {
        Task { [dispatcher] in await dispatcher.stopGuestServiceForTesting("win11") }  // else the channel retries a removed socket forever
        fake.stop()
        server.stop()
        try? FileManager.default.removeItem(atPath: home.path)
    }

    func run(_ argv: [String], timeout: Double? = nil, stdin: String? = nil) async throws -> Results.ExecRun {
        try await client.call(Method.execRun, Params.Exec(vm: "win11", argv: argv, stdin: stdin, timeout: timeout), as: Results.ExecRun.self)
    }

    func read(_ job: String, cursor: Int? = nil, maxBytes: Int? = nil) async throws -> Results.ExecRead {
        try await client.call(Method.execRead, Params.ExecRead(vm: "win11", jobId: job, cursor: cursor, maxBytes: maxBytes), as: Results.ExecRead.self)
    }

    /// The error a call fails with, or nil.
    func failure(_ method: String, _ params: JSONValue) async -> RPCError? {
        do { _ = try await client.call(method, params); return nil } catch let e as RPCError { return e } catch { return RPCError(code: 0, message: "\(error)") }
    }
}

extension JSONValue {
    var intValue: Int { if case .int(let i) = self { i } else { -1 } }
}

/// Stands in for the hypervisor boundary; all agent traffic still uses the real daemon socket and guest framing.
actor FixtureMachine: Machine {
    nonisolated let config: VMConfig
    var isRunning = true
    let processID: Int32?
    init(config: VMConfig, processID: Int32? = nil) { self.config = config; self.processID = processID }
    func guestIsRunning() async -> Bool { isRunning }
    func stop(timeout: Duration) async { isRunning = false }
}
