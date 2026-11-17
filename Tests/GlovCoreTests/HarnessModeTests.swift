import Foundation
import Testing

@testable import GlovCore

@Suite struct HarnessModeTests {
    let claude = try! HarnessProfile.named("claude")
    let codex = try! HarnessProfile.named("codex")
    let agy = try! HarnessProfile.named("antigravity")

    @Test func everyProfileHasAValidSubscription() {
        for p in HarnessProfile.all {
            #expect(p.subscription != nil, "\(p.name)")
            #expect(p.secretProblems.isEmpty, "\(p.name): \(p.secretProblems)")
            for host in p.subscription?.tokenEndpoints.map(\.host) ?? [] { #expect(p.signedIn(vm: "v").runtime.contains(host), "\(p.name): \(host)") }
            for s in p.subscription?.secrets ?? [] { #expect(Set(s.hosts).isSubset(of: p.signedIn(vm: "v").runtime), "\(p.name): \(s.name)") }
        }
    }

    @Test func signedInSwapsTheAPIKeyForTheVMsOwnSecrets() {
        let c = claude.signedIn(vm: "box")
        #expect(c.guestEnvironment["ANTHROPIC_API_KEY"] == nil)
        #expect(c.guestEnvironment["CLAUDE_CODE_OAUTH_TOKEN"] == "glov-secret:box.claude.token")
        #expect(c.guestEnvironment["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] == "1")
        let x = codex.signedIn(vm: "box")
        #expect(x.guestEnvironment["CODEX_API_KEY"] == nil)
        #expect(x.secrets.map(\.name) == ["box.codex.access", "box.codex.refresh"])
        #expect(x.runtime.contains("chatgpt.com") && x.runtime.contains("api.openai.com"))
    }

    @Test func theEnvironmentKeepsOtherLinesAndHoldsOneModesVariables() {
        let base = "PATH=/usr/bin\nHTTPS_PROXY=http://127.0.0.1:3128\nANTHROPIC_API_KEY=glov-secret:anthropic\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\nCODEX_API_KEY=glov-secret:openai\n"
        let on = HarnessMode.environment(base, profiles: [claude, codex], subscribed: ["claude"], vm: "box")
        #expect(on.hasPrefix("PATH=/usr/bin\nHTTPS_PROXY=http://127.0.0.1:3128\n"))
        #expect(!on.contains("ANTHROPIC_API_KEY") && on.contains("CLAUDE_CODE_OAUTH_TOKEN=glov-secret:box.claude.token\n"))
        #expect(on.contains("CODEX_API_KEY=glov-secret:openai\n"), "codex stays on its API key")
        let off = HarnessMode.environment(on, profiles: [claude, codex], subscribed: [], vm: "box")
        #expect(!off.contains("CLAUDE_CODE_OAUTH_TOKEN") && off.contains("ANTHROPIC_API_KEY=glov-secret:anthropic\n"))
        #expect(HarnessMode.environment(off, profiles: [claude, codex], subscribed: [], vm: "box") == off, "applying twice changes nothing")
    }

    @Test func onlyTheDifferingSettingsChangeSoTheOwnersEditsStay() throws {
        let config = try #require(codex.files.first { $0.path == ".codex/config.toml" }?.text)
        let edited = config.replacingOccurrences(of: "approval_policy = \"on-request\"", with: "approval_policy = \"never\"")
        let on = HarnessMode.edits(profiles: [codex], subscribed: ["codex"], vm: "box")
        #expect(on == [.init(path: ".codex/config.toml", key: ["model_provider"], value: "glov-chatgpt")])
        let signedIn = try #require(HarnessMode.apply(on, to: edited, path: ".codex/config.toml"))
        #expect(signedIn.contains("model_provider = \"glov-chatgpt\"\n") && signedIn.contains("approval_policy = \"never\""))
        #expect(signedIn.contains("[model_providers.glov]\n") && signedIn.contains("[model_providers.glov-chatgpt]\n"))
        let off = HarnessMode.edits(profiles: [codex], subscribed: [], vm: "box")
        #expect(HarnessMode.apply(off, to: signedIn, path: ".codex/config.toml") == edited)

        let settings = #"{"agentMode": "plan", "modelProvider": "gemini", "toolPermission": "always-proceed"}"#
        let agyOn = try #require(HarnessMode.apply(HarnessMode.edits(profiles: [agy], subscribed: ["antigravity"], vm: "box"), to: settings, path: ".gemini/antigravity-cli/settings.json"))
        #expect(!agyOn.contains("modelProvider") && agyOn.contains("\"agentMode\" : \"plan\""))
        let agyOff = try #require(HarnessMode.apply(HarnessMode.edits(profiles: [agy], subscribed: [], vm: "box"), to: agyOn, path: ".gemini/antigravity-cli/settings.json"))
        #expect(agyOff.contains("\"modelProvider\" : \"gemini\"") && agyOff.contains("\"agentMode\" : \"plan\""))
        // A file the owner broke is left alone rather than replaced.
        #expect(HarnessMode.apply(HarnessMode.edits(profiles: [agy], subscribed: [], vm: "box"), to: "{oops", path: ".gemini/antigravity-cli/settings.json") == nil)
    }

    @Test func tomlKeysAreSetAddedAndRemovedAtTheTopLevelOnly() {
        let text = "# c\nmodel = \"a\"\n\n[t]\nmodel = \"inner\"\n"
        #expect(HarnessMode.settingTOML(text, "model", "b") == "# c\nmodel = \"b\"\n\n[t]\nmodel = \"inner\"\n")
        #expect(HarnessMode.settingTOML(text, "model", nil) == "# c\n\n[t]\nmodel = \"inner\"\n")
        #expect(HarnessMode.settingTOML(text, "other", "x\"y") == "# c\nmodel = \"a\"\nother = \"x\\\"y\"\n\n[t]\nmodel = \"inner\"\n")
        #expect(HarnessMode.tomlValue(text, "model") == "a")
    }

    @Test func theMarkerListsTheSignedInHarnesses() {
        #expect(HarnessMode.marker(["codex", "claude"]) == "claude\ncodex\n")
        #expect(HarnessMode.parseMarker("claude\n\ncodex\n") == ["claude", "codex"])
    }
}
