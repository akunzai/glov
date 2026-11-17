import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct ControlTests {
    @Test func agentControlOffRefusesExecFilesCopyAndInputButNotScreenshots() async throws {
        let f = try await GuestServiceFixture(agentControl: false)
        defer { f.cleanup() }
        let calls: [(String, JSONValue)] = [
            (Method.execRun, ["vm": "win11", "argv": ["x"]]), (Method.execStart, ["vm": "win11", "argv": ["x"]]),
            (Method.execRead, ["vm": "win11", "jobId": "j1"]), (Method.execKill, ["vm": "win11", "jobId": "j1"]),
            (Method.fileRead, ["vm": "win11", "path": "C:\\a"]), (Method.fileWrite, ["vm": "win11", "path": "C:\\a", "text": ""]),
            (Method.copyToVM, ["vm": "win11", "hostPath": .string(f.shared), "guestPath": "C:\\a"]),
            (Method.copyFromVM, ["vm": "win11", "guestPath": "C:\\a", "hostPath": .string(f.shared + "/a")]),
            (Method.click, ["vm": "win11", "x": 1, "y": 1]), (Method.move, ["vm": "win11", "x": 1, "y": 1]),
            (Method.drag, ["vm": "win11", "fromX": 1, "fromY": 1, "toX": 2, "toY": 2]),
            (Method.type, ["vm": "win11", "text": "a"]), (Method.key, ["vm": "win11", "key": "enter"]),
            (Method.scroll, ["vm": "win11", "x": 1, "y": 1]),
        ]
        for (method, params) in calls {
            let error = await f.failure(method, params)
            #expect(error?.message.contains("agent control is disabled") == true, "\(method): \(error?.message ?? "no error")")
        }
        #expect(f.fake.requests.allSatisfy { $0.method == "guest.info" }, "nothing may reach the guest")
        // Screenshots are not input: they fail only because this fixture has no qemu.
        let shot = await f.failure(Method.screenshot, ["vm": "win11"])
        #expect(shot?.message == "VM is not running")
        // The Guest Service itself stays reachable for vm_wait.
        let waited = try await f.client.call(Method.vmWait, Params.Wait(vm: "win11", state: "guest-ready", timeout: 5), as: Results.VMInfo.self)
        #expect(waited.agent?.state == "ready")
    }

    @Test func turningAgentControlOffInVMJsonAppliesToTheNextCall() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        _ = try await f.run(["a"])
        var config = f.config
        config.agentControl = false
        let encoder = JSONEncoder()
        try encoder.encode(config).write(to: URL(fileURLWithPath: f.config.directory + "/vm.json"))
        let error = await f.failure(Method.execRun, ["vm": "win11", "argv": ["a"]])
        #expect(error?.message.contains("agent control is disabled") == true)
    }

    @Test func waitForGuestServiceReadyReportsTheGuestService() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.user = "alice"
        let waited = try await f.client.call(Method.vmWait, Params.Wait(vm: "win", state: "guest-ready", timeout: 5), as: Results.VMInfo.self)
        #expect(waited.agent?.state == "ready" && waited.agent?.os == "windows" && waited.agent?.version == "0.0-fake")
        #expect(waited.agent?.bootId == "boot-1")
    }

    @Test func waitForGuestServiceReadyTimesOutWithAnExplanation() async throws {
        let f = try await GuestServiceFixture(waitForGuestService: false)
        defer { f.cleanup() }
        f.fake.protocolVersion = 2
        // A wait reports what the daemon knows when it times out: on a busy runner the handshake can take longer than one
        // short wait, so wait again until it has refused the Guest Service.
        var error: RPCError?
        await eventually("the timeout never named the incompatible Guest Service") {
            error = await f.failure(Method.vmWait, ["vm": "win11", "state": "guest-ready", "timeout": .double(0.5)])
            return error?.message.contains("incompatible") == true
        }
        #expect(error?.message.contains("timed out waiting for the Guest Service") == true)
        #expect(error?.message.contains("protocol 2") == true)
    }

    @Test func aVMWithoutAGuestServiceSaysWhy() async throws {
        let off = try await GuestServiceFixture(agent: false)
        defer { off.cleanup() }
        #expect(await off.failure(Method.execRun, ["vm": "win11", "argv": ["x"]])?.message.contains("Guest Service disabled") == true)
        #expect(await off.failure(Method.vmWait, ["vm": "win11", "state": "guest-ready"])?.message.contains("Guest Service disabled") == true)
        // Enabled but never started (the VM is stopped).
        let stopped = try await GuestServiceFixture(waitForGuestService: false)
        defer { stopped.cleanup() }
        await stopped.dispatcher.stopGuestServiceForTesting("win11")
        let notRunning = await stopped.failure(Method.execRun, ["vm": "win11", "argv": ["x"]])
        #expect(notRunning?.message.contains("not running") == true, "\(String(describing: notRunning))")
    }

    @Test func copyArgumentsTellTheGuestSideFromADriveLetter() throws {
        let cwd = "/work"
        #expect(try CopyDirection.parse("proj", "win11:C:\\w", directory: cwd) == .toVM(vm: "win11", hostPath: "/work/proj", guestPath: "C:\\w"))
        #expect(try CopyDirection.parse("win11:C:\\out", "/tmp/o", directory: cwd) == .fromVM(vm: "win11", guestPath: "C:\\out", hostPath: "/tmp/o"))
        #expect(try CopyDirection.parse("C:\\only\\host?", "vm1:/x", directory: cwd) == .toVM(vm: "vm1", hostPath: "/work/C:\\only\\host?", guestPath: "/x"))
        #expect(throws: CopyDirection.Invalid.self) { try CopyDirection.parse("a", "b", directory: cwd) }
        #expect(throws: CopyDirection.Invalid.self) { try CopyDirection.parse("a:/x", "b:/y", directory: cwd) }
    }
}
