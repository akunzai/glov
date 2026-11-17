import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI
@testable import glov

private typealias Method = GlovDaemonAPI.Method

/// An open isolated VM (`egress.mode`): any public host through the proxy, the broker unchanged.
@Suite struct OpenEgressTests {
    struct Fixture {
        let home: GlovHome
        let dispatcher: Dispatcher

        init() async throws {
            home = GlovHome(path: try shortTempDir())
            try home.create(VMConfig(
                name: "box", directory: home.directory(of: "box"), engine: .vz, firmwareCode: "", tpm: false,
                isolated: true, egress: .init(allow: ["api.example.com"]), harnesses: ["t"]))
            try home.create(VMConfig(name: "plain", directory: home.directory(of: "plain"), engine: .vz, firmwareCode: "", tpm: false))
            dispatcher = Dispatcher(home: home, secrets: SecretBroker(home: home, values: MemoryValues()))
            await dispatcher.setHarnessProfilesForTesting([TmpfsSecretTests.profile])
            _ = try await call(Method.secretSet, Params.Secret(name: "cred", hosts: ["t.example.com"], value: "v"))
            _ = try await call(Method.secretSet, Params.Secret(name: "key", hosts: ["t.example.com"], value: "k"))
        }

        func call(_ method: String, _ params: some Encodable) async throws -> RPCResponse {
            await dispatcher.handle(.init(id: 1, method: method, params: try JSONValue(encoding: params)))
        }
    }

    @Test func openAndRestrictedSwitchAndKeepTheAllowlist() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.home.path) }
        let opened = try await f.call(Method.egressMode, Params.EgressMode(vm: "bo", open: true))
        #expect(try opened.result?.decode(Results.Egress.self) == Results.Egress(vm: "box", allow: ["api.example.com"], open: true))
        #expect(try f.home.loadConfig("box").egress?.open == true)
        let closed = try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: false))
        #expect(try closed.result?.decode(Results.Egress.self) == Results.Egress(vm: "box", allow: ["api.example.com"]))
        #expect(try f.home.loadConfig("box").egress?.open == nil)
        #expect(try await f.call(Method.egressMode, Params.EgressMode(vm: "plain", open: true)).error != nil)
    }

    @Test func plainSecretsNeedConfirmationToOpenOrToBeGrantedWhileOpen() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.home.path) }
        // A placeholder secret is no reason to ask.
        _ = try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: true))
        #expect(try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["key"])).error == nil)

        let refused = try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred"]))
        #expect(refused.error?.code == RPCError.plainSecrets)
        #expect(refused.error?.message == "VM box would hold cred as real values in tmpfs while open, and then any host could receive them; if that is acceptable, run glov secret grant box cred --allow-plain-secrets")
        #expect(try f.home.loadConfig("box").egress?.secrets == ["key"])
        #expect(try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred"], allowPlainSecrets: true)).error == nil)

        _ = try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: false))
        let reopen = try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: true))
        #expect(reopen.error?.code == RPCError.plainSecrets)
        #expect(reopen.error?.message.hasSuffix("run glov vm configure box --egress open --allow-plain-secrets") == true)
        #expect(try f.home.loadConfig("box").egress?.open == nil)
        #expect(try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: true, allowPlainSecrets: true)).error == nil)
        #expect(try f.home.loadConfig("box").egress?.open == true)
        // Restricting never asks.
        #expect(try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: false)).error == nil)
    }

    @Test func anOpenVMGetsOnlyTheTmpfsSecretsTheOwnerAllowed() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.home.path) }
        func held() async -> [String]? { await f.dispatcher.tmpfsSecretValues("box")?.map(\.name) }
        _ = try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred", "key"]))
        #expect(await held() == ["cred"])
        _ = try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: true, allowPlainSecrets: true))
        #expect(try f.home.loadConfig("box").egress?.plainSecrets == ["cred"])
        #expect(await held() == ["cred"])
        // Switching back forgets the permission, so reopening asks again.
        _ = try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: false))
        #expect(try f.home.loadConfig("box").egress?.plainSecrets == nil)
        #expect(await held() == ["cred"])
    }

    @Test func aHarnessAddedToAnOpenVMDoesNotSlipItsTmpfsSecretsIn() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.home.path) }
        // Granted and opened while no harness took cred in tmpfs, so nobody was asked.
        _ = try f.home.update("box") { $0.harnesses = nil }
        _ = try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred"]))
        #expect(try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: true)).error == nil)
        // glov vm harness add writes vm.json from the CLI, past every RPC check.
        _ = try f.home.update("box") { $0.harnesses = ["t"] }
        #expect(await f.dispatcher.tmpfsSecretValues("box")?.map(\.name) == [])
        _ = try await f.call(Method.egressMode, Params.EgressMode(vm: "box", open: true, allowPlainSecrets: true))
        #expect(await f.dispatcher.tmpfsSecretValues("box")?.map(\.name) == ["cred"])
    }

    @Test func aRefusalIsAskedAboutOnlyAtATerminalAndRetriedOnlyOnYes() async throws {
        let refusal = RPCError(code: RPCError.plainSecrets, message: "VM box would hold cred …")
        final class Sent: @unchecked Sendable { var flags: [Bool] = [] }
        func attempt(allow: Bool = false, terminal: Bool, answer: String?) async -> (sent: [Bool], asked: [String], ok: Bool) {
            let sent = Sent()
            var asked: [String] = []
            let ok = (try? await confirmingPlainSecrets(allow: allow, isTerminal: terminal, ask: { asked.append($0); return answer }) { flag in
                sent.flags.append(flag)
                if !flag { throw refusal }
                return true
            }) ?? false
            return (sent.flags, asked, ok)
        }
        let yes = await attempt(terminal: true, answer: " Y ")
        #expect(yes.sent == [false, true] && yes.asked == [refusal.message] && yes.ok)
        let no = await attempt(terminal: true, answer: "")
        #expect(no.sent == [false] && no.asked.count == 1 && !no.ok)
        let eof = await attempt(terminal: true, answer: nil)
        #expect(eof.sent == [false] && !eof.ok)
        let script = await attempt(terminal: false, answer: "y")
        #expect(script.sent == [false] && script.asked.isEmpty && !script.ok)
        let flagged = await attempt(allow: true, terminal: true, answer: "n")
        #expect(flagged.sent == [true] && flagged.asked.isEmpty && flagged.ok)
    }

    @Test func noMCPToolSwitchesTheModeAndVMConfigureCarriesNone() throws {
        #expect(!MCPTools.methods.values.contains(GlovDaemonAPI.Method.egressMode))
        // An MCP client's vm_configure arguments decode into Params.Configure, which has no egress field.
        let p = try JSONDecoder().decode(Params.Configure.self, from: Data(#"{"vm":"box","egress":"open","open":true}"#.utf8))
        #expect(p.cpus == nil && p.memory == nil && p.diskSize == nil && p.balloon == nil)
    }

    @Test func anOpenVMReachesAnyPublicHostButPrivateOnlyWhenAllowedSo() {
        let open = VMConfig.Egress(allow: ["gitlab.corp.example"], privateAllow: ["gitlab.corp.example"], open: true)
        #expect(open.permits(host: "example.org", port: 443))
        #expect(open.permits(host: "93.184.215.14", port: 22))
        #expect(!open.permits(host: "", port: 443))
        #expect(!open.permits(host: "example.org", port: 0))
        #expect(!open.reachesPrivateNetworks(host: "intranet.corp.example", port: 443))
        #expect(open.reachesPrivateNetworks(host: "gitlab.corp.example", port: 443))
        let restricted = VMConfig.Egress(allow: ["api.example.com"])
        #expect(!restricted.permits(host: "example.org", port: 443))
        #expect(restricted.permits(host: "api.example.com", port: 443))
    }
}
