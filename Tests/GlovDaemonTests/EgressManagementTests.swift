import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct EgressManagementTests {
    func makeHome() throws -> GlovHome {
        let home = GlovHome(path: try shortTempDir())
        for (name, isolated) in [("box", true), ("plain", false)] {
            try home.create(VMConfig(
                name: name, directory: home.directory(of: name), engine: .vz, firmwareCode: "", tpm: false,
                isolated: isolated, egress: isolated ? .init() : nil))
        }
        return home
    }

    func call(_ d: Dispatcher, _ method: String, _ params: some Encodable) async throws -> RPCResponse {
        await d.handle(.init(id: 1, method: method, params: try JSONValue(encoding: params)))
    }

    @Test func allowAndRemoveEditTheAllowlistInVMJson() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let d = Dispatcher(home: home)
        let allowed = try await call(d, Method.egressAllow, Params.Egress(vm: "bo", hosts: ["API.example.com", "*.ubuntu.com", "api.example.com"]))
        #expect(try allowed.result?.decode(Results.Egress.self) == Results.Egress(vm: "box", allow: ["api.example.com", "*.ubuntu.com"]))
        #expect(try home.loadConfig("box").egress?.allow == ["api.example.com", "*.ubuntu.com"])

        let removed = try await call(d, Method.egressRemove, Params.Egress(vm: "box", hosts: ["api.example.com"]))
        #expect(try removed.result?.decode(Results.Egress.self).allow == ["*.ubuntu.com"])
        let listed = try await call(d, Method.egressList, Params.Egress(vm: "box"))
        #expect(try listed.result?.decode(Results.Egress.self).allow == ["*.ubuntu.com"])

        let bad = try await call(d, Method.egressAllow, Params.Egress(vm: "box", hosts: ["a b"]))
        #expect(bad.error?.message.contains("not a valid egress entry") == true)
        let missing = try await call(d, Method.egressRemove, Params.Egress(vm: "box", hosts: ["nope.example.com"]))
        #expect(missing.error?.message == "not in the allowlist: nope.example.com")
        let empty = try await call(d, Method.egressAllow, Params.Egress(vm: "box", hosts: []))
        #expect(empty.error?.code == RPCError.invalidParams)
        #expect(try home.loadConfig("box").egress?.allow == ["*.ubuntu.com"])
    }

    @Test func onlyTheCLICanAllowAPrivateNetworkHost() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let d = Dispatcher(home: home)
        _ = try await call(d, Method.egressMCP, Params.EgressMCP(vm: "box", enabled: true))
        let refused = try await call(d, Method.egressAllow, Params.Egress(vm: "box", hosts: ["gitlab.corp.example"], mcp: true, private: true))
        #expect(refused.error?.message == "MCP clients cannot allow private-network hosts: ask the user to run glov egress allow box <host> --private")
        #expect(try home.loadConfig("box").egress?.allow == [])

        let allowed = try await call(d, Method.egressAllow, Params.Egress(vm: "box", hosts: ["gitlab.corp.example"], private: true))
        #expect(try allowed.result?.decode(Results.Egress.self)
            == Results.Egress(vm: "box", allow: ["gitlab.corp.example"], mcpEdits: true, private: ["gitlab.corp.example"]))
        let suffix = try await call(d, Method.egressAllow, Params.Egress(vm: "box", hosts: ["*.corp.example"], private: true))
        #expect(suffix.error?.message == "--private takes an exact hostname or IP address, not *.corp.example")
        _ = try await call(d, Method.egressRemove, Params.Egress(vm: "box", hosts: ["gitlab.corp.example"], mcp: true))
        #expect(try home.loadConfig("box").egress?.privateAllow == nil)
    }

    @Test func anMCPEditIsRefusedUntilTheVMOptsIn() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let d = Dispatcher(home: home)
        let refused = try await call(d, Method.egressAllow, Params.Egress(vm: "box", hosts: ["example.com"], mcp: true))
        #expect(refused.error?.message == "VM box does not let MCP clients change its egress allowlist: ask the user to run glov egress allow box ..., or to opt in with glov egress mcp box on")
        #expect(try home.loadConfig("box").egress?.allow == [])
        let opted = try await call(d, Method.egressMCP, Params.EgressMCP(vm: "box", enabled: true))
        #expect(try opted.result?.decode(Results.Egress.self) == Results.Egress(vm: "box", allow: [], mcpEdits: true))
        #expect(try home.loadConfig("box").egress?.mcpEdits == true)
        let allowed = try await call(d, Method.egressAllow, Params.Egress(vm: "box", hosts: ["example.com"], mcp: true))
        #expect(try allowed.result?.decode(Results.Egress.self).allow == ["example.com"])
        _ = try await call(d, Method.egressMCP, Params.EgressMCP(vm: "box", enabled: false))
        #expect(try home.loadConfig("box").egress?.mcpEdits == nil)
        #expect(try await call(d, Method.egressRemove, Params.Egress(vm: "box", hosts: ["example.com"], mcp: true)).error != nil)
        #expect(try await call(d, Method.egressRemove, Params.Egress(vm: "box", hosts: ["example.com"])).error == nil)
    }

    @Test func aVMThatIsNotIsolatedIsRefusedByEveryEgressMethod() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let d = Dispatcher(home: home)
        for method in [Method.egressList, Method.egressAllow, Method.egressRemove, Method.egressLog] {
            let r = try await call(d, method, Params.Egress(vm: "plain", hosts: ["example.com"]))
            #expect(r.error?.message == "VM plain is not isolated: egress policy applies only to VMs created with glov vm create --engine vz --isolated", "\(method)")
        }
        #expect(try home.loadConfig("plain").egress == nil)
    }

    @Test(.timeLimit(.minutes(1)))
    func glovEgressEditsListsAndShowsTheLog() async throws {
        let home = try makeHome()
        let server = try DaemonServer(home: home, dispatcher: Dispatcher(home: home))
        server.start()
        defer { server.stop(); try? FileManager.default.removeItem(atPath: home.path) }

        let empty = try await runglov(["egress", "list", "box"], home: home)
        #expect(empty.status == 0 && empty.out == "box: nothing allowed (every connection is denied)\n")
        let allowed = try await runglov(["egress", "allow", "box", "API.example.com", "*.ubuntu.com"], home: home)
        #expect(allowed.status == 0 && allowed.out == "api.example.com\n*.ubuntu.com\n")
        let removed = try await runglov(["egress", "remove", "box", "api.example.com"], home: home)
        #expect(removed.status == 0 && removed.out == "*.ubuntu.com\n")
        let refused = try await runglov(["egress", "list", "plain"], home: home)
        #expect(refused.status != 0 && refused.err.contains("VM plain is not isolated"))
        let invalid = try await runglov(["egress", "allow", "box", "a b"], home: home)
        #expect(invalid.status != 0 && invalid.err.contains("not a valid egress entry"))

        let opened = try await runglov(["vm", "configure", "box", "--egress", "open"], home: home)
        #expect(opened.status == 0 && opened.out == "box: egress open: any public host, through the egress proxy; the allowlist applies again after glov vm configure box --egress restricted\n", "\(opened)")
        let listedOpen = try await runglov(["egress", "list", "box"], home: home)
        #expect(listedOpen.out.hasPrefix("box: egress open:") && listedOpen.out.hasSuffix("\n*.ubuntu.com\n"), "\(listedOpen)")
        let restricted = try await runglov(["vm", "configure", "box", "--egress", "restricted"], home: home)
        #expect(restricted.status == 0 && restricted.out == "box: egress restricted: only the allowlist\n")
        #expect(try home.loadConfig("box").egress?.open == nil)
        // MCP's vm_configure cannot open it: its arguments have no egress field.
        let combined = try await runglov(["vm", "configure", "box", "--egress", "open", "--cpus", "2"], home: home)
        #expect(combined.status != 0 && combined.err.contains("pass --egress on its own"), "\(combined)")
        let stray = try await runglov(["vm", "configure", "box", "--egress", "restricted", "--allow-plain-secrets"], home: home)
        #expect(stray.status != 0 && stray.err.contains("--allow-plain-secrets goes with --egress open"), "\(stray)")

        let log = EgressLog(path: try home.loadConfig("box").egressLogPath)
        log.append(EgressRecord(time: Date(timeIntervalSince1970: 1_793_400_000), vm: "box", decision: .allow, host: "a.com", port: 443, bytesIn: 1, bytesOut: 2))
        log.append(EgressRecord(time: Date(timeIntervalSince1970: 1_793_400_001), vm: "box", decision: .deny, host: "b.com", port: 443, reason: "not in the allowlist"))
        let shown = try await runglov(["egress", "log", "box"], home: home)
        #expect(shown.status == 0 && shown.out == """
            2026-10-30T22:40:00Z allow a.com:443 in=1 out=2
            2026-10-30T22:40:01Z deny b.com:443 in=0 out=0 (not in the allowlist)

            """)
        let denied = try await runglov(["egress", "log", "box", "--denied"], home: home)
        #expect(denied.out == "2026-10-30T22:40:01Z deny b.com:443 in=0 out=0 (not in the allowlist)\n")

        let session = try MCPSession(home: home)
        defer { session.terminate() }
        _ = try await session.initialize()
        func tool(_ name: String, _ arguments: [String: Any]) async throws -> (text: String, isError: Bool) {
            let result = try await session.request("tools/call", ["name": name, "arguments": arguments])
            return ((result["content"] as? [[String: Any]])?.compactMap { $0["text"] as? String }.joined(separator: "\n") ?? "", result["isError"] as? Bool == true)
        }
        // MCP clients change the policy only on a VM whose user opted in, which only glov itself can do.
        _ = try await tool("vm_configure", ["vm": "box", "egress": "open"])
        #expect(try home.loadConfig("box").egress?.open == nil)
        let closed = try await tool("egress_allow", ["vm": "box", "hosts": ["example.com"]])
        #expect(closed.isError && closed.text.contains("glov egress mcp box on"))
        #expect(try home.loadConfig("box").egress?.allow == ["*.ubuntu.com"])
        let on = try await runglov(["egress", "mcp", "box", "on"], home: home)
        #expect(on.status == 0 && on.out == "box: MCP clients may change the egress allowlist\n")
        #expect(try await tool("egress_allow", ["vm": "box", "hosts": ["example.com"]]).text == "box allows: *.ubuntu.com, example.com")
        #expect(try await tool("egress_remove", ["vm": "box", "hosts": ["*.ubuntu.com"]]).text == "box allows: example.com")
        #expect(try await tool("egress_list", ["vm": "box"]).text == "box allows: example.com")
        let mcpLog = try await tool("egress_log", ["vm": "box", "denied": true])
        #expect(mcpLog.text.hasPrefix("2026-10-30T22:40:01Z deny b.com:443 in=0 out=0 (not in the allowlist)\ncursor: "))
        let off = try await runglov(["egress", "mcp", "box", "off"], home: home)
        #expect(off.status == 0 && off.out == "box: only glov may change the egress allowlist\n")
        #expect(try await tool("egress_remove", ["vm": "box", "hosts": ["example.com"]]).isError)
        let notIsolated = try await tool("egress_list", ["vm": "plain"])
        #expect(notIsolated.isError && notIsolated.text.contains("not isolated"))
    }

    @Test func logReturnsTheVMsRecordsFilteredAndFromACursor() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let log = EgressLog(path: try home.loadConfig("box").egressLogPath)
        log.append(EgressRecord(vm: "box", decision: .allow, host: "a.com", port: 443, bytesIn: 1, bytesOut: 2))
        log.append(EgressRecord(vm: "box", decision: .deny, host: "b.com", port: 443, reason: "not in the allowlist"))
        let d = Dispatcher(home: home)
        let all = try #require(try await call(d, Method.egressLog, Params.EgressLogRead(vm: "box")).result?.decode(Results.EgressLogPage.self))
        #expect(all.records.map(\.host) == ["a.com", "b.com"])
        let denied = try #require(try await call(d, Method.egressLog, Params.EgressLogRead(vm: "box", denied: true)).result?.decode(Results.EgressLogPage.self))
        #expect(denied.records.map(\.host) == ["b.com"])
        log.append(EgressRecord(vm: "box", decision: .deny, host: "c.com", port: 80, reason: "not in the allowlist"))
        let next = try #require(try await call(d, Method.egressLog, Params.EgressLogRead(vm: "box", cursor: all.cursor)).result?.decode(Results.EgressLogPage.self))
        #expect(next.records.map(\.host) == ["c.com"])
        let limited = try #require(try await call(d, Method.egressLog, Params.EgressLogRead(vm: "box", limit: 1)).result?.decode(Results.EgressLogPage.self))
        #expect(limited.records.map(\.host) == ["c.com"])
    }
}
