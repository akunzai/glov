import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// `secret.login` / `secret.logout` switching a running guest (a fake Guest Service, no VM).
@Suite struct SubscriptionLoginTests {
    static let codex = HarnessProfile.all.first { $0.name == "codex" }!
    static let claude = HarnessProfile.all.first { $0.name == "claude" }!
    static let configPath = "/home/\(CloudInitSeed.user)/.codex/config.toml"

    struct Fixture {
        let home: GlovHome
        let dispatcher: Dispatcher
        let fake: FakeGuestService

        init() async throws {
            home = GlovHome(path: try shortTempDir())
            try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
            try home.create(VMConfig(
                name: "box", directory: home.directory(of: "box"), firmwareCode: home.path + "/code.fd", tpm: false,
                isolated: true, egress: .init(allow: ["api.openai.com"]), harnesses: ["claude", "codex"]))
            let config = try home.loadConfig("box")
            fake = try FakeGuestService(socketPath: config.guestSocketPath)
            fake.os = "linux"
            let profiles = [SubscriptionLoginTests.claude, SubscriptionLoginTests.codex]
            // As cloud-init left it: API mode, and the owner changed a setting since.
            fake.files = [
                "/etc/environment": Data(HarnessMode.environment("PATH=/usr/bin\n", profiles: profiles, subscribed: [], vm: "box").utf8),
                SubscriptionLoginTests.configPath: Data(SubscriptionLoginTests.codex.files.first { $0.path == ".codex/config.toml" }!.content
                    .replacingOccurrences(of: "approval_policy = \"on-request\"", with: "approval_policy = \"never\"").utf8),
            ]
            dispatcher = Dispatcher(home: home, secrets: SecretBroker(home: home, values: MemoryValues()))
            await dispatcher.startGuestService(config)
            for _ in 0..<400 where await dispatcher.guestServiceForTesting("box")?.isReady != true { try await Task.sleep(for: .milliseconds(25)) }
        }

        func call(_ method: String, _ params: some Encodable) async throws -> RPCResponse {
            await dispatcher.handle(.init(id: 1, method: method, params: try JSONValue(encoding: params)))
        }

        func file(_ path: String) -> String { fake.files[path].map { String(decoding: $0, as: UTF8.self) } ?? "" }

        func cleanup() async {
            await dispatcher.stopGuestServiceForTesting("box")
            fake.stop()
            try? FileManager.default.removeItem(atPath: home.path)
        }
    }

    @Test(.timeLimit(.minutes(1))) func codexSignsInInTheGuestAndOutAgainLeavingTheOwnersSettings() async throws {
        let f = try await Fixture()
        // No marker means API mode, so the session's own check leaves the guest alone.
        try await Task.sleep(for: .milliseconds(200))
        #expect(f.fake.files[HarnessMode.markerPath] == nil && f.fake.files["/etc/environment"] != nil)

        let r = try await f.call(Method.secretLogin, Params.SecretLogin(vm: "box", harness: "codex"))
        #expect(r.error == nil, "\(String(describing: r.error))")
        let login = try #require(r.result).decode(Results.SecretLogin.self)
        #expect(login.command == ["codex", "login", "--device-auth"] && login.missing == ["box.codex.access", "box.codex.refresh"])
        let egress = try #require(try f.home.loadConfig("box").egress)
        #expect(egress.subscriptions == ["codex"] && egress.secrets == ["box.codex.access", "box.codex.refresh"])
        #expect(egress.allow.contains("auth.openai.com") && egress.allow.contains("chatgpt.com"))
        #expect(f.file(HarnessMode.markerPath) == "codex\n")
        #expect(!f.file("/etc/environment").contains("CODEX_API_KEY") && f.file("/etc/environment").contains("PATH=/usr/bin"))
        #expect(f.file(Self.configPath).contains("model_provider = \"glov-chatgpt\"") && f.file(Self.configPath).contains("approval_policy = \"never\""))

        #expect(try await f.call(Method.secretLogout, Params.SecretLogin(vm: "box", harness: "codex")).error == nil)
        let after = try #require(try f.home.loadConfig("box").egress)
        #expect(after.subscriptions == nil && after.secrets == nil && after.allow == ["api.openai.com"])
        #expect(f.file("/etc/environment").contains("CODEX_API_KEY=glov-secret:openai"))
        #expect(!f.file(Self.configPath).contains("model_provider = \"glov-chatgpt\"") && f.file(Self.configPath).contains("approval_policy = \"never\""))
        #expect(f.file(HarnessMode.markerPath) == "")
        let again = try await f.call(Method.secretLogout, Params.SecretLogin(vm: "box", harness: "codex")).error?.message
        #expect(again?.contains("glov secret login box codex") == true)
        await f.cleanup()
    }

    @Test(.timeLimit(.minutes(1))) func aPastedClaudeTokenIsStoredUnderTheVMAndTheGuestGetsItsPlaceholder() async throws {
        let f = try await Fixture()
        let missing = try await f.call(Method.secretLogin, Params.SecretLogin(vm: "box", harness: "claude")).error?.message
        #expect(missing?.contains("claude setup-token") == true)
        let r = try await f.call(Method.secretLogin, Params.SecretLogin(vm: "box", harness: "claude", value: "sk-ant-oat-VALUE"))
        let login = try #require(r.result).decode(Results.SecretLogin.self)
        #expect(login.command == nil && login.missing.isEmpty)
        let list = try #require(try await f.call(Method.secretList, JSONValue.object([:])).result).decode(Results.Secrets.self)
        #expect(list.secrets.map(\.name) == ["box.claude.token"])
        let environment = f.file("/etc/environment")
        #expect(environment.contains("CLAUDE_CODE_OAUTH_TOKEN=glov-secret:box.claude.token") && !environment.contains("ANTHROPIC_API_KEY"))
        #expect(!environment.contains("VALUE"))

        // A guest away at the switch catches up at its next session.
        f.fake.files[HarnessMode.markerPath] = Data()
        f.fake.files["/etc/environment"] = Data("PATH=/usr/bin\n".utf8)
        f.fake.dropConnection()
        // The marker is written last.
        for _ in 0..<400 where f.file(HarnessMode.markerPath).isEmpty { try await Task.sleep(for: .milliseconds(25)) }
        #expect(f.file(HarnessMode.markerPath) == "claude\n")
        #expect(f.file("/etc/environment").contains("CLAUDE_CODE_OAUTH_TOKEN=glov-secret:box.claude.token"))
        await f.cleanup()
    }

    @Test func aHarnessTheVMLacksIsRefused() async throws {
        let f = try await Fixture()
        let message = try await f.call(Method.secretLogin, Params.SecretLogin(vm: "box", harness: "antigravity")).error?.message
        #expect(message?.hasPrefix("VM box has no harness antigravity") == true)
        await f.cleanup()
    }
}
