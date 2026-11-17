import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// A daemon as `glov host install` sees it: canned answers per method, and every call recorded.
private final class FakeDaemon: @unchecked Sendable {
    private let lock = NSLock()
    private var _calls: [(String, JSONValue)] = []
    var answer: @Sendable (String, JSONValue) throws -> JSONValue = { _, _ in .null }
    var calls: [(String, JSONValue)] { lock.withLock { _calls } }
    func methods(_ method: String) -> [JSONValue] { calls.filter { $0.0 == method }.map(\.1) }
    var rpc: DaemonReload.RPC {
        { [self] method, params in
            lock.withLock { _calls.append((method, params)) }
            return try answer(method, params)
        }
    }
}

private func vm(_ name: String, engine: String, state: String = "running", agent: String = "ready", busy: [String]? = []) -> Results.VMInfo {
    .init(name: name, state: state, agent: .init(state: agent, busy: busy), engine: engine)
}

private func list(_ vms: Results.VMInfo...) throws -> JSONValue { try JSONValue(encoding: Results.List(vms: vms)) }

private func reload(_ d: FakeDaemon, log: Box = Box("")) -> DaemonReload {
    DaemonReload(rpc: d.rpc, log: { line in log.value += line + "\n" }, timeout: 2)
}

@Suite struct DaemonReloadTests {
    @Test func impactNamesLinuxVMsAndRunningWorkOnEveryVM() async throws {
        let d = FakeDaemon()
        d.answer = { _, _ in
            try list(
                vm("ubuntu", engine: "vz"), vm("lin", engine: "vz", agent: "disabled", busy: nil),
                vm("win11", engine: "qemu", busy: ["job j3", "share sync proj"]), vm("old", engine: "qemu", busy: nil),
                vm("off", engine: "vz", state: "stopped"), vm("idle", engine: "qemu"))
        }
        let impact = try await reload(d).impact()
        #expect(impact.linuxVMs == ["ubuntu", "lin"])
        #expect(impact.withoutGuestService == ["lin"])
        #expect(impact.work == ["win11: job j3", "win11: share sync proj", "old: running work unknown (the running daemon predates this check)"])
        #expect(impact.summary.contains("shut down: ubuntu, lin") && impact.summary.contains("cut off: win11: job j3"))
        d.answer = { _, _ in try list(vm("idle", engine: "qemu")) }
        #expect(try await reload(d).impact().isEmpty)
    }

    @Test func asksOnATerminalAndRefusesWithoutOneUnlessYes() {
        let affected = DaemonReload.Impact(linuxVMs: ["ubuntu"])
        let never: (String) -> String? = { _ in Issue.record("asked"); return nil }
        #expect(DaemonReload.decide(nil, verb: "installing", yes: false, isTerminal: false, ask: never) == .proceed)
        #expect(DaemonReload.decide(.init(), verb: "installing", yes: false, isTerminal: false, ask: never) == .proceed)
        #expect(DaemonReload.decide(affected, verb: "installing", yes: true, isTerminal: false, ask: never) == .proceed)
        guard case .refused(let message) = DaemonReload.decide(affected, verb: "installing", yes: false, isTerminal: false, ask: never) else {
            Issue.record("not refused"); return
        }
        #expect(message.contains("ubuntu") && message.contains("--yes"))
        let asked = Box("")
        #expect(DaemonReload.decide(affected, verb: "installing", yes: false, isTerminal: true) { asked.value = $0; return "y" } == .proceed)
        #expect(asked.value.contains("ubuntu") && asked.value.contains("Installing anyway? [y/N]"))
        #expect(DaemonReload.decide(affected, verb: "installing", yes: false, isTerminal: true) { _ in "" } == .declined)
    }

    @Test func stopsCleanlyAndRestartsEachVMUntilUsable() async throws {
        let d = FakeDaemon()
        d.answer = { method, params in
            switch method {
            case Method.vmStart where params["vm"] == .string("broken"): throw ProvisionError("no disk")
            default: return .null
            }
        }
        let r = reload(d)
        #expect(await r.stop(["ubuntu", "lin"]).allSatisfy { !$0.failed })
        #expect(d.methods(Method.vmStop).map { $0["force"] } == [nil, nil])  // as vm stop does: ACPI first
        let outcomes = await r.restart(.init(linuxVMs: ["broken", "lin", "plain"], withoutGuestService: ["plain"]))
        #expect(outcomes.map(\.result) == ["failed: no disk", "ready", "ready"])
        #expect(d.methods(Method.vmWait).map { $0["state"] } == [.string("guest-ready"), .string("running")])
        #expect(try d.methods(Method.vmWait).map { try $0.decode(Params.Wait.self).timeout } == [2, 2], "glovd waits, for the whole timeout")
    }

    @Test func updatesOnlyIdleOutdatedGuestServicesAndNeverForces() async throws {
        let d = FakeDaemon()
        d.answer = { method, params in
            let name = params["vm"].flatMap { if case .string(let s) = $0 { s } else { nil } } ?? ""
            switch method {
            case Method.vmList:
                return try list(
                    vm("current", engine: "qemu"), vm("busy", engine: "qemu", busy: ["job j1"]), vm("old", engine: "vz"),
                    vm("racing", engine: "qemu"), vm("broken", engine: "qemu"), vm("down", engine: "qemu", agent: "unresponsive"))
            case Method.guestServiceCheck:
                return try JSONValue(encoding: Results.GuestServiceCheck(state: name == "current" ? "up-to-date" : "update-available"))
            case Method.guestServiceUpdate:
                if name == "racing" { throw RPCError(code: RPCError.failed, message: "job j9; wait for them or use --force to allow interruption") }
                if name == "broken" { throw RPCError(code: RPCError.failed, message: "copy failed") }
                return try JSONValue(encoding: GuestServiceUpdate.Outcome.updated(from: "a", to: "b"))
            default: return .null
            }
        }
        let log = Box("")
        let outcomes = await reload(d, log: log).updateGuestServices()
        #expect(outcomes.map(\.vm) == ["busy", "old", "racing", "broken"])
        #expect(outcomes[0].result.hasPrefix("skipped (job j1 running)") && outcomes[0].result.contains("glov vm guest update busy") && !outcomes[0].failed)
        #expect(outcomes[1].result == "updated a → b")
        #expect(outcomes[2].result.hasPrefix("skipped") && !outcomes[2].failed)
        #expect(outcomes[3].failed)
        #expect(d.methods(Method.guestServiceUpdate).allSatisfy { $0["force"] == nil })
        #expect(!d.methods(Method.guestServiceCheck).contains { $0["vm"] == .string("down") })
        #expect(log.value.contains("Updating busy Guest Service... skipped"))
        #expect(DaemonReload.summary(outcomes).contains("old guest update: updated a → b"))
    }
}

@Suite struct GuestServiceBusyTests {
    @Test(.timeLimit(.minutes(1))) func statusListsRunningJobsAsWorkARestartWouldCut() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        #expect(await a.status().busy == [])
        f.fake.script = { _ in [.hold] }
        let started: Results.JobStarted = try await f.client.call(Method.execStart, Params.Exec(vm: "win11", argv: ["hold"]))
        await eventually { await a.status().busy == ["job \(started.jobId)"] }
        let info: Results.VMInfo = try await f.client.call(Method.vmStatus, Params.VM(vm: "win11"))
        #expect(info.agent?.busy == ["job \(started.jobId)"])
    }
}
