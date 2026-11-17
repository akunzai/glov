import Foundation
import Testing

@testable import GlovCore

@Suite struct HarnessProfileTests {
    @Test func everyProfileLoadsWithItsVerifiedVersionDocsAndAllowlists() throws {
        #expect(HarnessProfile.all.map(\.name) == ["antigravity", "claude", "codex"])
        for p in HarnessProfile.all {
            #expect(!p.verified.version.isEmpty && p.verified.date.wholeMatch(of: /\d{4}-\d{2}-\d{2}/) != nil, "\(p.name)")
            #expect(!p.docs.isEmpty && p.docs.allSatisfy { $0.hasPrefix("https://") }, "\(p.name)")
            for entry in p.runtime + p.install { #expect(EgressPolicy.canonical(entry) == entry, "\(p.name): \(entry)") }
            #expect(Set(p.runtime).isSubset(of: p.install), "\(p.name): the install allowlist adds to the runtime one")
            #expect(p.secretProblems.isEmpty, "\(p.name): \(p.secretProblems)")
            // Each secret's hosts are allowed at run time, or nothing would carry it there.
            for secret in p.secrets { #expect(Set(secret.hosts).isSubset(of: p.runtime), "\(p.name): \(secret.name)") }
            #expect(!p.environment.values.contains { $0.hasPrefix("glov-secret:") }, "\(p.name): placeholders come from secrets")
            #expect(!p.guestEnvironment.values.contains { $0.contains("127.0.0.1:3128") }, "\(p.name): no in-guest model endpoint")
            // Run as the guest user; the command ends up on the default PATH, where glov exec finds it.
            #expect(!p.command.isEmpty && p.installScript.contains("/usr/local/bin/\(p.command)"), "\(p.name)")
            // In `curl ... | sh` a failed download feeds the shell nothing and the script still exits 0.
            #expect(p.installScript.wholeMatch(of: /(?s).*\|\s*(ba|z)?sh\b.*/) == nil, "\(p.name): download the installer to a file first")
            for f in p.files { #expect(!f.path.hasPrefix("/") && !f.path.split(separator: "/").contains(".."), "\(p.name): \(f.path)") }
        }
        let paths = HarnessProfile.all.flatMap { $0.files.map(\.path) }
        #expect(Set(paths).count == paths.count, "two profiles write one file")
        let keys = HarnessProfile.all.flatMap(\.guestEnvironment.keys)
        #expect(Set(keys).count == keys.count, "two profiles set one variable")
    }

    @Test func claudeUsesItsSecretAndTurnsOffNonessentialTraffic() throws {
        let p = try HarnessProfile.named("claude")
        #expect(p.secrets == [.init(name: "anthropic", delivery: .placeholder, env: "ANTHROPIC_API_KEY", hosts: ["api.anthropic.com"])])
        #expect(p.environment["ANTHROPIC_BASE_URL"] == nil)
        #expect(p.guestEnvironment["ANTHROPIC_API_KEY"] == "glov-secret:anthropic")
        #expect(p.environment["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] == "1")
        #expect(p.files.map(\.path) == [".claude.json", ".claude/settings.json"])
        // An interactive session (glov claude) skips the onboarding, whose connection check is denied.
        #expect(p.files.first { $0.path == ".claude.json" }?.json?["hasCompletedOnboarding"] == .bool(true))
        // ... and does not ask to approve the placeholder key: claude keeps the last 20 characters of an approved key.
        let key = p.guestEnvironment["ANTHROPIC_API_KEY"] ?? ""
        #expect(p.files.first { $0.path == ".claude.json" }?.json?["customApiKeyResponses"]?["approved"] == .array([.string(String(key.suffix(20)))]))
        let settings = p.files.first { $0.path == ".claude/settings.json" }
        #expect(settings?.json?["skipWebFetchPreflight"] == .bool(true))
        // Unattended runs: nobody answers a prompt, so claude's automatic review decides.
        #expect(settings?.json?["permissions"]?["defaultMode"] == "auto")
        #expect(p.runtime == ["api.anthropic.com"])
        #expect(p.install.contains("downloads.claude.ai"))
    }

    @Test func codexUsesItsSecretAndTurnsOffAnalyticsFeedbackAndUpdateChecks() throws {
        let p = try HarnessProfile.named("codex")
        #expect(p.secrets == [.init(name: "openai", delivery: .placeholder, env: "CODEX_API_KEY", hosts: ["api.openai.com"])])
        let config = try #require(p.files.first { $0.path == ".codex/config.toml" }?.text)
        // A provider of its own: the built-in one tries a WebSocket the proxy does not relay.
        #expect(config.contains("model_provider = \"glov\"\n"))
        #expect(config.contains("base_url = \"https://api.openai.com/v1\"\nenv_key = \"CODEX_API_KEY\"\n"))
        #expect(config.contains("supports_websockets = false\n"))
        // codex's sandbox, with its automatic review deciding what needs more.
        #expect(config.contains("sandbox_mode = \"workspace-write\"\napproval_policy = \"on-request\"\napprovals_reviewer = \"auto_review\"\n"))
        #expect(p.userNamespaces)
        #expect(config.range(of: "approvals_reviewer")!.lowerBound < config.range(of: "[")!.lowerBound)
        #expect(config.contains("check_for_update_on_startup = false\n"))
        #expect(config.contains("[analytics]\nenabled = false\n"))
        #expect(config.contains("[feedback]\nenabled = false\n"))
        // Top-level keys come before the first table, or TOML puts them in it.
        #expect(config.range(of: "model_provider =")!.lowerBound < config.range(of: "[")!.lowerBound)
        #expect(config.range(of: "check_for_update_on_startup")!.lowerBound < config.range(of: "[")!.lowerBound)
        #expect(p.guestEnvironment["CODEX_API_KEY"] == "glov-secret:openai")
        #expect(p.runtime == ["api.openai.com"])
    }

    @Test func antigravityUsesItsSecretAndSaysWhatItCannotTurnOff() throws {
        let p = try HarnessProfile.named("antigravity")
        #expect(p.secrets == [.init(name: "gemini", delivery: .placeholder, env: "GEMINI_API_KEY", hosts: ["generativelanguage.googleapis.com"])])
        #expect(p.environment["GOOGLE_GEMINI_BASE_URL"] == nil)
        #expect(p.guestEnvironment["GEMINI_API_KEY"] == "glov-secret:gemini")
        #expect(p.runtime == ["generativelanguage.googleapis.com"])
        let settings = try #require(p.files.first { $0.path == ".gemini/antigravity-cli/settings.json" }?.json)
        #expect(settings["modelProvider"] == "gemini")
        #expect(settings["agentMode"] == "accept-edits" && settings["toolPermission"] == "always-proceed")
        #expect(p.notes.contains { $0.contains("telemetry") } && p.notes.contains { $0.contains("update") })
        #expect(p.install.contains("antigravity.google") && p.install.contains { $0.hasSuffix(".run.app") })
    }

    private func profile(_ secrets: String, files: String = "[]") throws -> HarnessProfile {
        try JSONDecoder().decode(HarnessProfile.self, from: Data("""
            {"name": "t", "title": "T", "command": "t", "verified": {"version": "1", "date": "2026-11-03"}, "docs": [],
             "secrets": \(secrets), "environment": {"A": "1"}, "files": \(files), "runtime": [], "install": [],
             "installScript": "", "userNamespaces": false, "notes": []}
            """.utf8))
    }

    @Test func secretsBecomeVariablesJSONValuesAndLinks() throws {
        let p = try profile("""
            [{"name": "k", "delivery": "placeholder", "env": "K", "hosts": ["h.example"]},
             {"name": "j", "delivery": "placeholder", "file": ".t/s.json", "json": ["auth", "token"], "hosts": ["h.example"]},
             {"name": "n", "delivery": "placeholder", "file": ".t/new.json", "json": ["key"], "hosts": ["h.example"]},
             {"name": "c", "delivery": "tmpfs", "file": ".t/cred.json", "hosts": ["h.example"]},
             {"name": "p", "delivery": "tmpfs", "env": "P_FILE", "hosts": ["h.example"]}]
            """, files: #"[{"path": ".t/s.json", "json": {"auth": {"mode": "key"}, "x": 1}}]"#)
        #expect(p.secretProblems.isEmpty)
        #expect(p.guestEnvironment == ["A": "1", "K": "glov-secret:k", "P_FILE": "/run/glov/secrets/p"])
        #expect(p.guestFiles.map(\.path) == [".t/s.json", ".t/new.json"])
        #expect(p.guestFiles[0].json == ["auth": ["mode": "key", "token": "glov-secret:j"], "x": 1])
        #expect(p.guestFiles[1].json == ["key": "glov-secret:n"])
        #expect(p.secretLinks.map { "\($0.path) -> \($0.target)" } == [".t/cred.json -> /run/glov/secrets/c"])
        let needs = p.secretNeeds(vm: "box", defined: ["k"])
        #expect(needs[0] == "harness t: secret k (a placeholder in K, sent to h.example) is defined, not granted: glov secret grant box k")
        #expect(needs[1] == "harness t: secret j (a placeholder in ~/.t/s.json at auth.token, sent to h.example) is not defined or granted: glov secret set j --host h.example, then glov secret grant box j")
        #expect(needs[3].hasPrefix("harness t: secret c (the value in /run/glov/secrets/c, linked from ~/.t/cred.json, sent to h.example)"))
        #expect(p.secretNeeds(vm: "box", defined: nil)[0].hasSuffix("is not granted: glov secret grant box k (define it first if glov secret list lacks it: glov secret set k --host h.example)"))
    }

    @Test func malformedSecretsAreNamed() throws {
        let p = try profile("""
            [{"name": "a", "delivery": "placeholder", "hosts": ["h.example"]},
             {"name": "b", "delivery": "placeholder", "file": ".t/x.json", "hosts": ["h.example"]},
             {"name": "c", "delivery": "tmpfs", "file": ".t/y", "json": ["k"], "hosts": ["h.example"]},
             {"name": "d", "delivery": "placeholder", "env": "A", "hosts": []},
             {"name": "e", "delivery": "placeholder", "file": ".t/t.toml", "json": ["k"], "hosts": ["h.example"]},
             {"name": "e", "delivery": "tmpfs", "file": ".t/t.toml", "hosts": ["h.example"]}]
            """, files: #"[{"path": ".t/t.toml", "text": "x = 1\n"}]"#)
        #expect(p.secretProblems == [
            "a: name one target, env or file",
            "b: a placeholder in a file goes at a json path",
            "c: a tmpfs secret is the whole file; it takes no json path",
            "d: name the hosts it is sent to",
            "d: A is also in environment",
            "e: .t/t.toml is not JSON",
            "e: .t/t.toml is also one of the profile's files",
            "a secret is declared twice",
        ])
    }

    @Test func anUnknownHarnessNamesTheKnownOnes() {
        #expect(throws: HarnessProfile.Unknown(name: "aider")) { try HarnessProfile.named("aider") }
        #expect("\(HarnessProfile.Unknown(name: "aider"))" == "unknown harness aider: use antigravity, claude or codex")
    }
}
