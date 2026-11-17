import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// A daemon in this process with a stopped QEMU VM ("win11") and a stopped VZ VM ("ubuntu").
private struct MixedFixture {
    let home: GlovHome
    let server: DaemonServer
    var client: DaemonClient { DaemonClient(socketPath: home.socketPath) }

    init() throws {
        home = GlovHome(path: try shortTempDir())
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        try home.create(VMConfig(name: "win11", directory: home.directory(of: "win11"), firmwareCode: home.path + "/code.fd", tpm: false))
        try home.create(VMConfig(name: "ubuntu", directory: home.directory(of: "ubuntu"), engine: .vz, firmwareCode: "", tpm: false))
        server = try DaemonServer(home: home, dispatcher: Dispatcher(home: home))
        server.start()
    }

    func cleanup() {
        server.stop()
        try? FileManager.default.removeItem(atPath: home.path)
    }

    func failure(_ method: String, _ params: JSONValue) async -> RPCError? {
        do { _ = try await client.call(method, params); return nil } catch let e as RPCError { return e } catch { return RPCError(code: 0, message: "\(error)") }
    }
}

@Suite struct VZRoutingTests {
    @Test func listAndStatusShowTheEngine() async throws {
        let f = try MixedFixture()
        defer { f.cleanup() }
        let list = try await f.client.call(Method.vmList).decode(Results.List.self)
        #expect(list.vms.map { [$0.name, $0.state, $0.engine ?? ""] } == [["ubuntu", "stopped", "vz"], ["win11", "stopped", "qemu"]])
        #expect(try await f.client.call(Method.vmStatus, Params.VM(vm: "ub"), as: Results.VMInfo.self).engine == "vz")
    }

    @Test func aVZVMNeedsNoFirmwareAndGetsNoBlankVarsFile() throws {
        let f = try MixedFixture()
        defer { f.cleanup() }
        let c = try f.home.loadConfig("ubuntu")
        #expect(c.engine == .vz)
        #expect(!FileManager.default.fileExists(atPath: c.firmwareVars))  // VZ creates its own store on first start
        #expect(FileManager.default.fileExists(atPath: try f.home.loadConfig("win11").firmwareVars))
    }

    @Test func snapshotToolsWorkOnAStoppedVZVM() async throws {
        let f = try MixedFixture()
        defer { f.cleanup() }
        let dir = f.home.directory(of: "disky")
        try f.home.create(VMConfig(name: "disky", directory: dir, engine: .vz, disk: dir + "/disk.img", firmwareCode: "", tpm: false))
        let config = try f.home.loadConfig("disky")
        try Data("before".utf8).write(to: URL(fileURLWithPath: dir + "/disk.img"))
        try Data("vars".utf8).write(to: URL(fileURLWithPath: config.firmwareVars))
        func list() async throws -> [SnapshotInfo] {
            try await f.client.call(Method.snapshotList, ["vm": "disky"]).decode(Results.SnapshotList.self).snapshots
        }
        #expect(try await list().isEmpty)

        let created = try await f.client.call(Method.snapshotCreate, ["vm": "disky", "name": "clean"]).decode(Results.SnapshotList.self)
        #expect(created.snapshots.map(\.name) == ["clean"])
        #expect(created.snapshots.first?.live == false)
        #expect(await f.failure(Method.snapshotCreate, ["vm": "disky", "name": "clean"])?.message.contains("already exists") == true)
        #expect(await f.failure(Method.snapshotCreate, ["vm": "disky", "name": "no/slash"])?.message.contains("invalid snapshot name") == true)

        try Data("after".utf8).write(to: URL(fileURLWithPath: dir + "/disk.img"))
        let reverted = try await f.client.call(Method.snapshotRevert, ["vm": "disky", "name": "clean"]).decode(Results.VMInfo.self)
        #expect(reverted.state == "stopped")
        #expect(FileManager.default.contents(atPath: dir + "/disk.img") == Data("before".utf8))
        #expect(await f.failure(Method.snapshotRevert, ["vm": "disky", "name": "nope"])?.message.contains("no snapshot") == true)

        _ = try await f.client.call(Method.snapshotDelete, ["vm": "disky", "name": "clean"])
        #expect(try await list().isEmpty)
        // A VZ VM without a disk answers like a QEMU one.
        #expect(await f.failure(Method.snapshotList, ["vm": "ubuntu"])?.message.contains("no system disk") == true)
    }

    @Test func revertingAnIsolatedVMRestoresTheEgressPolicyOfItsSnapshot() async throws {
        let f = try MixedFixture()
        defer { f.cleanup() }
        let dir = f.home.directory(of: "iso")
        let taken = VMConfig.Egress(allow: ["api.anthropic.com"], secrets: ["anthropic", "github", "iso.claude.token"], subscriptions: ["claude"])
        try f.home.create(VMConfig(name: "iso", directory: dir, engine: .vz, disk: dir + "/disk.img", firmwareCode: "", tpm: false, isolated: true, egress: taken))
        try Data("disk".utf8).write(to: URL(fileURLWithPath: dir + "/disk.img"))
        try Data("vars".utf8).write(to: URL(fileURLWithPath: try f.home.loadConfig("iso").firmwareVars))
        _ = try await f.client.call(Method.snapshotCreate, ["vm": "iso", "name": "ready"])

        // A task widened the allowlist and was granted a secret; one was revoked since.
        _ = try await f.client.call(Method.egressAllow, ["vm": "iso", "hosts": ["github.com"]])
        // Since the snapshot, claude was signed out and codex signed in (its tokens granted, its hosts allowed).
        try f.home.update("iso") {
            $0.egress?.secrets = ["github", "npm", "iso.codex.access", "iso.codex.refresh"]
            $0.egress?.subscriptions = ["codex"]
            $0.egress?.allow += ["auth.openai.com", "chatgpt.com"]
        }
        _ = try await f.client.call(Method.snapshotRevert, ["vm": "iso", "name": "ready"])
        // The allowlist is the snapshot's and of its grants only those still held stay, so a revoke outlasts the revert;
        // the sign-ins are today's, with their grants and hosts, whichever came first.
        #expect(try f.home.loadConfig("iso").egress == .init(
            allow: ["api.anthropic.com", "auth.openai.com", "chatgpt.com"], secrets: ["github", "iso.codex.access", "iso.codex.refresh"], subscriptions: ["codex"]))
        try f.home.update("iso") { $0.egress?.secrets = nil; $0.egress?.subscriptions = nil }
        _ = try await f.client.call(Method.snapshotRevert, ["vm": "iso", "name": "ready"])
        #expect(try f.home.loadConfig("iso").egress?.secrets == nil && f.home.loadConfig("iso").egress?.subscriptions == nil)

        // A snapshot from before snapshots kept the policy leaves the current one alone.
        try FileManager.default.removeItem(atPath: VZSnapshots.directory(try f.home.loadConfig("iso"), "ready") + "/" + VZSnapshots.egressFile)
        _ = try await f.client.call(Method.egressAllow, ["vm": "iso", "hosts": ["github.com"]])
        _ = try await f.client.call(Method.snapshotRevert, ["vm": "iso", "name": "ready"])
        #expect(try f.home.loadConfig("iso").egress?.allow == ["api.anthropic.com", "github.com"])
    }

    @Test func screenAndInputToolsReachAStoppedVZVMLikeAQEMUOne() async throws {
        let f = try MixedFixture()
        defer { f.cleanup() }
        let calls: [(String, JSONValue)] = [
            (Method.screenshot, ["vm": "ubuntu"]),
            (Method.click, ["vm": "ubuntu", "x": 1, "y": 1]),
            (Method.move, ["vm": "ubuntu", "x": 1, "y": 1]),
            (Method.drag, ["vm": "ubuntu", "fromX": 1, "fromY": 1, "toX": 2, "toY": 2]),
            (Method.scroll, ["vm": "ubuntu", "x": 1, "y": 1, "dy": 1]),
            (Method.type, ["vm": "ubuntu", "text": "x"]),
            (Method.key, ["vm": "ubuntu", "key": "enter"]),
        ]
        for (method, params) in calls {
            #expect(await f.failure(method, params) == RPCError(code: RPCError.failed, message: "VM is not running"), "\(method)")
            var qemu = params
            if case .object(var o) = qemu { o["vm"] = "win11"; qemu = .object(o) }
            #expect(await f.failure(method, qemu) == RPCError(code: RPCError.failed, message: "VM is not running"), "\(method) on QEMU")
        }
    }

    @Test func agentControlOffRefusesVZInputButNotScreenshots() async throws {
        let f = try MixedFixture()
        defer { f.cleanup() }
        try f.home.create(VMConfig(name: "locked", directory: f.home.directory(of: "locked"), engine: .vz, firmwareCode: "", tpm: false, agentControl: false))
        #expect(await f.failure(Method.key, ["vm": "locked", "key": "enter"])?.message.contains("agent control is disabled") == true)
        #expect(await f.failure(Method.screenshot, ["vm": "locked"]) == RPCError(code: RPCError.failed, message: "VM is not running"))
    }

    @Test func guestServiceToolsOnAStoppedVZVMSayItIsNotRunning() async throws {
        let f = try MixedFixture()
        defer { f.cleanup() }
        let e = await f.failure(Method.execRun, ["vm": "ubuntu", "argv": ["/bin/true"]])
        #expect(e?.message.contains("is not running") == true)
        #expect(await f.failure(Method.vmStop, ["vm": "ubuntu"]) == RPCError(code: RPCError.failed, message: "VM is not running"))
    }

    @Test func startingAVZVMWithoutTheEntitlementNamesTheFix() async throws {
        guard !VZConfiguration.hasVirtualizationEntitlement else { return }  // a signed runner would really boot; covered manually
        let f = try MixedFixture()
        defer { f.cleanup() }
        let e = await f.failure(Method.vmStart, ["vm": "ubuntu"])
        #expect(e?.message.contains("com.apple.security.virtualization") == true)
        #expect(e?.message.contains("tools/linux-vm/sign-daemon.sh") == true)
        #expect(try await f.client.call(Method.vmStatus, Params.VM(vm: "ubuntu"), as: Results.VMInfo.self).state == "stopped")
    }
}
