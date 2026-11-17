import Foundation
import Testing

@testable import GlovDaemonAPI

/// `glov --help` and `glov guide` as child processes; neither needs a daemon.
@Suite struct CLIHelpTests {
    private func glov(_ arguments: [String]) async throws -> (status: Int32, out: String, err: String) {
        try await runglov(arguments, home: GlovHome(path: try shortTempDir()))
    }

    private func skillFile(_ path: String) throws -> String {
        let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        return try String(contentsOf: root.appendingPathComponent("skills/glov/" + path), encoding: .utf8)
    }

    @Test(.timeLimit(.minutes(1)))
    func versionNamesTheBuildAndTheUngroupedCommandsAreGone() async throws {
        let flag = try await glov(["--version"])
        #expect(flag.status == 0 && !flag.out.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, "\(flag.out)")
        let version = try await glov(["version"])
        #expect(version.status == 0 && version.out.hasPrefix("glov \(flag.out.trimmingCharacters(in: .whitespacesAndNewlines)) (protocol "), "\(version.out)")
        #expect(version.out.contains("glovd"), "\(version.out)")
        for old in ["screenshot", "stats", "guest", "harness", "api-key", "daemon", "media"] {
            #expect(try await glov([old]).status != 0, "\(old) should now live under vm, egress or host")
        }
    }

    @Test(.timeLimit(.minutes(1)))
    func everyArgumentAndOptionHasAHelpLine() async throws {
        let dump = try await glov(["--experimental-dump-help"])
        #expect(dump.status == 0)
        let tree = try #require(try JSONSerialization.jsonObject(with: Data(dump.out.utf8)) as? [String: Any])
        var missing: [String] = []
        func walk(_ command: [String: Any], _ path: [String]) {
            for case let argument as [String: Any] in command["arguments"] as? [Any] ?? [] where argument["shouldDisplay"] as? Bool ?? true {
                let abstract = argument["abstract"] as? String ?? ""
                let names = (argument["names"] as? [[String: Any]])?.compactMap { $0["name"] as? String }.joined(separator: "/")
                if abstract.isEmpty { missing.append((path + [names ?? argument["valueName"] as? String ?? "?"]).joined(separator: " ")) }
            }
            for case let sub as [String: Any] in command["subcommands"] as? [Any] ?? [] {
                // ArgumentParser's own `help` subcommand takes a bare positional.
                if sub["commandName"] as? String != "help" { walk(sub, path + [sub["commandName"] as? String ?? "?"]) }
            }
        }
        walk(try #require(tree["command"] as? [String: Any]), ["glov"])
        #expect(missing.isEmpty, "arguments without a help string: \(missing)")
    }

    @Test(.timeLimit(.minutes(1)))
    func guidePrintsTheBuiltInSkillAndItsReferences() async throws {
        let skill = try await glov(["guide"])
        let skillText = try skillFile("SKILL.md")
        #expect(skill.status == 0 && skill.out == skillText)
        let troubleshooting = try await glov(["guide", "troubleshooting"])
        let troubleshootingText = try skillFile("references/troubleshooting.md")
        #expect(troubleshooting.status == 0 && troubleshooting.out == troubleshootingText)
        let unknown = try await glov(["guide", "nope"])
        #expect(unknown.status != 0 && unknown.out.isEmpty && unknown.err.contains("troubleshooting"))
    }

    @Test(.timeLimit(.minutes(1)))
    func execHelpStatesTheGuestSideRulesAndTheNoUserExitCode() async throws {
        let help = try await glov(["exec", "--help"])
        #expect(help.status == 0)
        let text = help.out.replacingOccurrences(of: "\n", with: " ").replacingOccurrences(of: "  ", with: " ")
        #expect(text.contains("must already exist") && text.contains("exits 75"))
    }

    @Test(.timeLimit(.minutes(1)))
    func rebootHelpStatesTheWaitTheRefusalsAndTheTimeout() async throws {
        let help = try await glov(["vm", "reboot", "--help"])
        #expect(help.status == 0)
        let text = help.out.replacingOccurrences(of: "\n", with: " ").replacingOccurrences(of: "  ", with: " ")
        #expect(text.contains("new boot") && text.contains("600"), "\(text)")
        #expect(text.contains("--force") && text.contains("not ready"), "\(text)")
        #expect(text.contains("glov vm screenshot") && text.contains("never forces"), "\(text)")
    }
}
