import Foundation
import Testing

@testable import GlovCore

@Suite struct ProjectAgentTests {
    @Test func theProjectIsTheNearestAncestorWithGitElseTheDirectory() throws {
        let base = try #require(HostPath.realpath(NSTemporaryDirectory())) + "/glov-project-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: base) }
        let fm = FileManager.default
        try fm.createDirectory(atPath: base + "/repo/.git", withIntermediateDirectories: true)
        try fm.createDirectory(atPath: base + "/repo/src/deep", withIntermediateDirectories: true)
        try fm.createDirectory(atPath: base + "/worktree/sub", withIntermediateDirectories: true)
        try Data("gitdir: /elsewhere\n".utf8).write(to: URL(fileURLWithPath: base + "/worktree/.git"))
        try fm.createDirectory(atPath: base + "/plain/x", withIntermediateDirectories: true)
        try fm.createSymbolicLink(atPath: base + "/link", withDestinationPath: base + "/repo/src")

        #expect(ProjectAgent.root(of: base + "/repo/src/deep") == base + "/repo")
        #expect(ProjectAgent.root(of: base + "/repo") == base + "/repo")
        #expect(ProjectAgent.root(of: base + "/worktree/sub") == base + "/worktree")
        #expect(ProjectAgent.root(of: base + "/link") == base + "/repo")
        #expect(ProjectAgent.root(of: base + "/plain/x") == base + "/plain/x")
    }

    @Test func aWorktreeNeedsItsRepositorysCommonGitDirectory() async throws {
        let base = try #require(HostPath.realpath(NSTemporaryDirectory())) + "/glov-wt-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: base) }
        func run(_ args: [String], _ dir: String) async throws {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/usr/bin/git")
            p.arguments = ["-C", dir, "-c", "user.name=t", "-c", "user.email=t@t.invalid"] + args
            p.standardOutput = FileHandle.nullDevice
            p.standardError = FileHandle.nullDevice
            try await runToExit(p)
            #expect(p.terminationStatus == 0, "\(args)")
        }
        try FileManager.default.createDirectory(atPath: base + "/main", withIntermediateDirectories: true)
        try await run(["init", "-q"], base + "/main")
        try await run(["commit", "-q", "--allow-empty", "-m", "init"], base + "/main")
        try await run(["worktree", "add", "-q", base + "/wt"], base + "/main")
        #expect(ProjectAgent.gitDirectory(project: base + "/wt") == base + "/main/.git")
        #expect(ProjectAgent.gitDirectory(project: base + "/main") == nil)
        #expect(ProjectAgent.gitDirectory(project: base) == nil)

        // A .git file is the project's (or a former guest's) to write: it must not pick a host folder to share.
        let fm = FileManager.default
        try fm.createDirectory(atPath: base + "/secret/worktrees/x", withIntermediateDirectories: true)
        for (name, gitdir) in [
            ("elsewhere", base + "/secret"),                  // not a worktree's directory
            ("fake", base + "/secret/worktrees/x"),           // no repository there, no back-link
            ("other", base + "/main/.git/worktrees/wt"),      // a real worktree's directory, but not this project's
        ] {
            try fm.createDirectory(atPath: base + "/" + name, withIntermediateDirectories: true)
            try Data("gitdir: \(gitdir)\n".utf8).write(to: URL(fileURLWithPath: base + "/" + name + "/.git"))
            #expect(ProjectAgent.gitDirectory(project: base + "/" + name) == nil, "\(name)")
        }
        // A back-link alone does not make a repository of the folder above.
        try Data((base + "/fake/.git\n").utf8).write(to: URL(fileURLWithPath: base + "/secret/worktrees/x/gitdir"))
        #expect(ProjectAgent.gitDirectory(project: base + "/fake") == nil)
    }

    @Test func theProjectsRemotesAreItsVMsRepositories() throws {
        let base = try #require(HostPath.realpath(NSTemporaryDirectory())) + "/glov-project-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: base) }
        try FileManager.default.createDirectory(atPath: base + "/.git", withIntermediateDirectories: true)
        try "[remote \"origin\"]\n\turl = git@github.com:Owner/App.git\n[remote \"up\"]\n\turl = https://github.com/owner/app\n[remote \"local\"]\n\turl = /srv/app.git\n"
            .write(toFile: base + "/.git/config", atomically: true, encoding: .utf8)
        #expect(ProjectAgent.remoteRepos(project: base).map(\.text) == ["github.com/owner/app"])
        #expect(ProjectAgent.remoteRepos(project: base + "/missing").isEmpty)
    }

    @Test func theVMNameIsAValidNameFromTheFolderAndAHashOfThePath() {
        let a = ProjectAgent.vmName(project: "/Users/me/src/My App_v2")
        #expect(a.hasPrefix("my-app-v2-") && a.count == "my-app-v2-".count + 6)
        #expect(ProjectAgent.vmName(project: "/Users/me/src/My App_v2") == a)
        #expect(ProjectAgent.vmName(project: "/Users/me/other/My App_v2") != a)
        #expect(ProjectAgent.vmName(project: "/Users/me/.hidden").hasPrefix("hidden-"))
        #expect(ProjectAgent.vmName(project: "/Users/me/日本").hasPrefix("project-"))
        #expect(ProjectAgent.vmName(project: "/Users/me/" + String(repeating: "a", count: 40)).hasPrefix(String(repeating: "a", count: 24) + "-"))
        for path in ["/Users/me/src/My App_v2", "/Users/me/.hidden", "/Users/me/日本", "/Users/me/-x-"] {
            let name = ProjectAgent.vmName(project: path)
            #expect(!name.hasPrefix(".") && !name.hasPrefix("-") && !name.contains("--"), "\(name)")
            #expect(name.unicodeScalars.allSatisfy { CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyz0123456789-").contains($0) }, "\(name)")
        }
    }

    @Test func aNewSessionTakesTheFirstFreeNumber() {
        #expect(ProjectAgent.session(harness: "claude", existing: ["claude", "claude-2"], new: false) == "claude")
        #expect(ProjectAgent.session(harness: "claude", existing: [], new: true) == "claude-2")
        #expect(ProjectAgent.session(harness: "claude", existing: ["claude", "claude-2", "claude-4"], new: true) == "claude-3")
    }

    @Test func theTmuxCommandQuotesEveryWordForTheRemoteShell() {
        #expect(ProjectAgent.tmuxCommand(server: "claude", session: "claude", directory: "/Users/me/My App", argv: ["claude", "--model", "it's; rm -rf ~"])
            == "exec tmux -L claude new-session -A -s claude -c '/Users/me/My App' -- claude --model 'it'\\''s; rm -rf ~'")
        #expect(ProjectAgent.shellQuote("") == "''")
        #expect(ProjectAgent.shellQuote("$HOME") == "'$HOME'")
    }

    @Test func theFirstDoubleDashIsGlovsAndTheRestGoToTheHarness() {
        #expect(ProjectAgent.harnessArguments(["--", "--version"]) == ["--version"])
        #expect(ProjectAgent.harnessArguments(["-c"]) == ["-c"])
        #expect(ProjectAgent.harnessArguments(["--model", "x", "--", "--new"]) == ["--model", "x", "--new"])
        #expect(ProjectAgent.harnessArguments(["--", "exec", "--", "ls"]) == ["exec", "--", "ls"])
    }

    @Test func aHarnessIsNamedByItsCommandOrItsProfileName() {
        #expect(ProjectAgent.harness(for: "agy")?.name == "antigravity")
        #expect(ProjectAgent.harness(for: "antigravity")?.name == "antigravity")
        #expect(ProjectAgent.harness(for: "claude")?.command == "claude")
        #expect(ProjectAgent.harness(for: "vm") == nil)
        // Every profile's command is a word glov can take without shadowing one of its own commands.
        for p in HarnessProfile.all { #expect(!p.command.isEmpty && !p.command.contains("/"), "\(p.name)") }
    }
}
