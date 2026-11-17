import Foundation
import Testing

@testable import GlovCore

@Suite struct HostConfigTests {
    func tempDir() throws -> String {
        let path = FileManager.default.temporaryDirectory.appendingPathComponent("hostconfig-" + UUID().uuidString).path
        try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
        return (path as NSString).resolvingSymlinksInPath
    }

    func write(_ path: String, _ text: String) throws {
        try FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
        try Data(text.utf8).write(to: URL(fileURLWithPath: path))
    }

    @Test func linksArriveAsPlainFilesAndBrokenLinksCyclesAndCredentialsAreLeftOut() throws {
        let root = try tempDir(), staging = try tempDir()
        defer { for d in [root, staging] { try? FileManager.default.removeItem(atPath: d) } }
        let home = root + "/home", fm = FileManager.default
        try write(root + "/shared/AGENTS.md", "be brief")
        try write(root + "/shared/skills/review/SKILL.md", "review")
        try write(root + "/shared/skills/review/.credentials.json", "{}")
        try fm.createDirectory(atPath: home + "/.claude/skills", withIntermediateDirectories: true)
        try fm.createSymbolicLink(atPath: home + "/.claude/CLAUDE.md", withDestinationPath: root + "/shared/AGENTS.md")
        try fm.createSymbolicLink(atPath: home + "/.claude/skills/review", withDestinationPath: root + "/shared/skills/review")
        try fm.createSymbolicLink(atPath: home + "/.claude/skills/gone", withDestinationPath: root + "/nowhere")
        try fm.createSymbolicLink(atPath: home + "/.claude/skills/loop", withDestinationPath: home + "/.claude/skills")
        let copies = [
            HarnessProfile.HostCopy(host: ".claude/CLAUDE.md", guest: ".claude/CLAUDE.md"),
            .init(host: ".claude/skills", guest: ".claude/skills"),
            .init(host: ".claude/agents", guest: ".claude/agents"),
        ]
        let report = try HostConfig.stage(copies, hostHome: home, into: staging)
        #expect(report.copied.map(\.guest) == [".claude/CLAUDE.md", ".claude/skills"])
        #expect(report.files == 2 && report.bytes == 14)
        #expect(report.skipped == ["~/.claude/skills/gone (broken link)", "~/.claude/skills/loop (link cycle)", "~/.claude/skills/review/.credentials.json (credentials)"])
        #expect(try String(contentsOfFile: staging + "/.claude/CLAUDE.md", encoding: .utf8) == "be brief")
        #expect((try? fm.destinationOfSymbolicLink(atPath: staging + "/.claude/skills/review")) == nil)
        #expect(try String(contentsOfFile: staging + "/.claude/skills/review/SKILL.md", encoding: .utf8) == "review")
    }

    @Test func aCopyOverTheLimitStopsAndSaysWhere() throws {
        let home = try tempDir(), staging = try tempDir()
        defer { for d in [home, staging] { try? FileManager.default.removeItem(atPath: d) } }
        try Data(count: HostConfig.maxBytes + 1).write(to: URL(fileURLWithPath: home + "/big"))
        #expect(throws: HostConfig.TooLarge.self) { try HostConfig.stage([.init(host: "big", guest: "big")], hostHome: home, into: staging) }
    }

    @Test func theFormatRefusesCredentialsEscapesAndWhatGlovWrites() {
        let files = [HarnessProfile.File(path: ".codex/config.toml", json: nil, text: "")]
        #expect(HostConfig.problems([
            .init(host: ".codex/auth.json", guest: ".codex/auth.json"),
            .init(host: "../etc", guest: "/etc"),
            .init(host: ".codex", guest: ".codex"),
            .init(host: ".codex/AGENTS.md", guest: ".codex/AGENTS.md"),
        ], files: files) == [
            "hostConfig .codex/auth.json: holds credentials, which are never copied",
            "hostConfig .codex/auth.json: holds credentials, which are never copied",
            "hostConfig ../etc: give a relative path inside the home directory",
            "hostConfig /etc: give a relative path inside the home directory",
            "hostConfig .codex: overlaps .codex/config.toml, which glov writes",
        ])
        for p in HarnessProfile.all { #expect(!(p.hostConfig ?? []).isEmpty, "\(p.name)") }
    }
}
