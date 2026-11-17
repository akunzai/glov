import Foundation
import Testing

@testable import GlovCore

@Suite struct ProjectGitTests {
    @Test func theFirstRunsAnswerPicksGitByDefault() {
        #expect(ProjectGit.choice("") == .git)
        #expect(ProjectGit.choice(" Git ") == .git)
        #expect(ProjectGit.choice("live") == .live)
        #expect(ProjectGit.choice("y") == nil)
    }

    static let home = "/Users/me/Library/Application Support/glov"
    static func host(_ vm: String) -> ProjectGit.SSHHost {
        .init(vm: vm, key: "\(home)/vms/\(vm)/secrets/id_ed25519", knownHosts: "\(home)/vms/\(vm)/known_hosts", relay: "'/opt/glov' ssh-relay '\(vm)'")
    }

    @Test func eachVMHasOneBlockThatKeepsTheGuestFromTheMac() {
        let one = ProjectGit.withSSHHost("", Self.host("a-123456"))
        #expect(one.hasPrefix("# glov a-123456: begin (written by glov)\nHost glov-a-123456\n  HostName a-123456\n  User glov\n"))
        // A path with a space is one value.
        #expect(one.contains("  IdentityFile \"\(Self.home)/vms/a-123456/secrets/id_ed25519\"\n"))
        for option in ["ClearAllForwardings yes", "ForwardAgent no", "EnableEscapeCommandline no", "ControlMaster no", "ProxyCommand '/opt/glov' ssh-relay 'a-123456'"] {
            #expect(one.contains("  \(option)\n"), "\(option)")
        }
        #expect(ProjectGit.hasSSHHost(one, vm: "a-123456") && !ProjectGit.hasSSHHost(one, vm: "a"))
        // Written again it replaces its block; another VM's stays.
        let two = ProjectGit.withSSHHost(ProjectGit.withSSHHost(one, Self.host("b-654321")), Self.host("a-123456"))
        #expect(two.components(separatedBy: "Host glov-a-123456\n").count == 2)
        #expect(ProjectGit.hasSSHHost(two, vm: "b-654321"))
        let without = ProjectGit.withoutSSHHost(two, vm: "a-123456")
        #expect(!without.contains("a-123456") && ProjectGit.hasSSHHost(without, vm: "b-654321"))
    }

    @Test func fileOptionsDoubleAPercentSign() {
        #expect(ProjectGit.quoted("/tmp/100% sure") == "\"/tmp/100%% sure\"")
    }

    @Test func theIncludeIsWrittenFromHomeAndFoundQuotedOrNot() {
        let path = Self.home + "/ssh_config"
        #expect(ProjectGit.includeLine(path: path, home: "/Users/me") == "Include \"~/Library/Application Support/glov/ssh_config\"")
        #expect(ProjectGit.includeLine(path: "/opt/glov/ssh_config", home: "/Users/me") == "Include \"/opt/glov/ssh_config\"")
        let added = ProjectGit.withInclude("Host *\n  ServerAliveInterval 30\n", path: path, home: "/Users/me")
        // First: after a Host line it would apply only within that host.
        #expect(added.split(separator: "\n")[1] == "Include \"~/Library/Application Support/glov/ssh_config\"")
        #expect(added.hasSuffix("Host *\n  ServerAliveInterval 30\n"))
        #expect(ProjectGit.hasInclude(added, path: path, home: "/Users/me"))
        #expect(ProjectGit.hasInclude("  include \"\(path)\" ~/.ssh/other\n", path: path, home: "/Users/me"))
        // Unquoted, ssh would read two paths.
        #expect(!ProjectGit.hasInclude("Include ~/Library/Application Support/glov/ssh_config\n", path: path, home: "/Users/me"))
        #expect(!ProjectGit.hasInclude("# Include \"\(path)\"\n", path: path, home: "/Users/me"))
    }

    @Test func aSignInWrittenIntoTheOriginStaysOnTheMac() {
        #expect(ProjectGit.withoutCredentials("https://me:ghp_x@github.com/o/r.git") == "https://github.com/o/r.git")
        #expect(ProjectGit.withoutCredentials("https://oauth2@gitlab.example.com/o/r.git") == "https://gitlab.example.com/o/r.git")
        #expect(ProjectGit.withoutCredentials("https://github.com/o/r.git") == "https://github.com/o/r.git")
        #expect(ProjectGit.withoutCredentials("https://ghp_x@github.com/o/r.git") == "https://github.com/o/r.git")
        #expect(ProjectGit.withoutCredentials("ssh://git@github.com/o/r.git") == "ssh://git@github.com/o/r.git")
        #expect(ProjectGit.withoutCredentials("git@github.com:o/r.git") == "git@github.com:o/r.git")
        #expect(ProjectGit.withoutCredentials("") == "")
    }

    @Test func gitCherryListsTheCommitsUpstreamLacks() {
        #expect(ProjectGit.cherryPicks("- aaa\n+ bbb\n+ ccc\n") == ["bbb", "ccc"])
        #expect(ProjectGit.cherryPicks("") == [])
    }

    @Test func aProjectVMMadeBeforeModesSharesLive() throws {
        var config = VMConfig(name: "p-123456", directory: "/tmp/p", engine: .vz, firmwareCode: "", isolated: true)
        #expect(config.projectSync(path: "/Users/me/p") == nil && !config.isProjectVM)
        config.shares = [VMConfig.Share(name: ProjectAgent.shareName, hostPath: "/Users/me/p", guestPath: "/Users/me/p", mode: .rw)]
        #expect(config.projectSync(path: "/Users/me/p") == .live && config.isProjectVM)
        #expect(config.projectSync(path: "/Users/me/q") == nil)
        config.shares = []
        config.project = .init(path: "/Users/me/p", sync: .git)
        #expect(config.projectSync(path: "/Users/me/p") == .git && config.isProjectVM)
        #expect(config.projectSync(path: "/Users/me/q") == nil)
        let decoded = try JSONDecoder().decode(VMConfig.self, from: JSONEncoder().encode(config))
        #expect(decoded.project == config.project)
    }

    /// The guest's repository takes a push to its checked-out branch into the working tree, commits as the owner,
    /// unsigned, and a second run keeps what it has.
    @Test func theGuestRepositoryTakesPushesIntoItsWorkingTree() async throws {
        let root = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: root) }
        let mac = root + "/mac", guest = root + "/guest"
        try FileManager.default.createDirectory(atPath: guest, withIntermediateDirectories: true)
        try await git(["init", "-q", "-b", "work", mac])
        try Data("one\n".utf8).write(to: URL(fileURLWithPath: mac + "/a.txt"))
        try await git(["-C", mac, "add", "a.txt"])
        try await git(["-C", mac, "-c", "user.name=t", "-c", "user.email=t@t.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "one"])
        for _ in 0..<2 {
            #expect(try await sh(ProjectGit.guestRepoScript, [guest, "work", "Owner", "owner@example.com", "https://example.com/r.git"]) == 0)
        }
        #expect(try await git(["-C", guest, "config", "commit.gpgsign"]) == "false")
        #expect(try await git(["-C", guest, "config", "user.email"]) == "owner@example.com")
        #expect(try await git(["-C", guest, "remote", "get-url", "origin"]) == "https://example.com/r.git")
        try await git(["-C", mac, "push", "-q", guest, "work:work"])
        #expect(try String(contentsOfFile: guest + "/a.txt", encoding: .utf8) == "one\n")
        // The branch script switches to a branch the Mac pushed.
        try await git(["-C", mac, "push", "-q", guest, "work:other"])
        #expect(try await sh(ProjectGit.guestBranchScript, [guest, "other"]) == 0)
        #expect(try await git(["-C", guest, "symbolic-ref", "--short", "HEAD"]) == "other")
    }

    @discardableResult
    private func git(_ arguments: [String]) async throws -> String {
        try await run("/usr/bin/env", ["git"] + arguments)
    }

    private func sh(_ script: String, _ arguments: [String]) async throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = ["-c", script, "sh"] + arguments
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        try await runToExit(p)
        return p.terminationStatus
    }

    private func run(_ path: String, _ arguments: [String]) async throws -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = arguments
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        try await runToExit(p)
        let data = await Blocking.run { out.fileHandleForReading.readDataToEndOfFile() }
        guard p.terminationStatus == 0 else { throw CocoaError(.executableLoad) }
        return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    }
}
