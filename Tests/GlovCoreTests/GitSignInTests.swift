import Foundation
import Testing

@testable import GlovCore

@Suite struct GitSignInTests {
    let gh = GitSignIn.Entry(name: "gh", forge: .github, hosts: ["github.com", "api.github.com", "uploads.github.com"])
    let gl = GitSignIn.Entry(name: "gl", forge: .gitlab, hosts: ["gitlab.example.com:443", "*.example.com", "10.0.0.1", "gitlab.example.com:8443"])

    @Test func gitHostsFoldAPIHostsAndSkipWhatNamesNoGitHost() {
        #expect(gh.gitHosts == ["github.com"])
        #expect(gl.gitHosts == ["gitlab.example.com"])
        #expect(GitSignIn.signIns([gl, gh]) == "github.com github gh\ngitlab.example.com gitlab gl\n")
    }

    @Test func gitConfigKeepsTheOwnersLinesAndReplacesItsBlock() {
        let owner = "[core]\n\teditor = vim\n"
        let once = GitSignIn.gitConfig(owner, entries: [gh, gl])
        #expect(once.hasPrefix(owner))
        #expect(once.contains("[credential]\n\thelper = glov\n"))
        #expect(once.contains("[url \"https://gitlab.example.com/\"]\n\tinsteadOf = git@gitlab.example.com:\n\tinsteadOf = ssh://git@gitlab.example.com/\n"))
        #expect(GitSignIn.gitConfig(once, entries: [gh, gl]) == once, "idempotent")
        #expect(!GitSignIn.gitConfig(once, entries: [gh]).contains("gitlab"))
        #expect(GitSignIn.gitConfig(once, entries: []) == owner)
    }

    @Test func cliConfigsHoldPlaceholdersOnly() {
        let hosts = GitSignIn.ghHosts([gh, gl])!
        #expect(hosts.contains("github.com:\n") && hosts.contains("oauth_token: glov-secret:gh") && !hosts.contains("gitlab"))
        let glab = GitSignIn.glabConfig([gh, gl])!
        #expect(glab.contains("host: gitlab.example.com\nhosts:\n    gitlab.example.com:\n") && glab.contains("token: glov-secret:gl"))
        #expect(glab.contains("        oauth2_expiry_date: 01 Jan 68 00:00 UTC\n"))
        #expect(GitSignIn.ghHosts([gl]) == nil && GitSignIn.glabConfig([gh]) == nil)
    }

    @Test func theHelperAnswersOnlyHTTPSHostsItKnows() async throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let signIns = dir.appendingPathComponent("sign-ins")
        try GitSignIn.signIns([gh, gl]).write(to: signIns, atomically: true, encoding: .utf8)
        let script = dir.appendingPathComponent("helper")
        try GitSignIn.helper.replacingOccurrences(of: GitSignIn.signInsPath, with: signIns.path).write(to: script, atomically: true, encoding: .utf8)
        func ask(_ input: String, _ action: String = "get") async throws -> String {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/bin/sh")
            p.arguments = [script.path, action]
            let stdin = Pipe(), stdout = Pipe()
            p.standardInput = stdin
            p.standardOutput = stdout
            let (exited, exit) = AsyncStream<Void>.makeStream()
            p.terminationHandler = { _ in exit.yield(); exit.finish() }
            try p.run()
            // Pipe I/O blocks, so off the cooperative pool; `try?` because a helper that answers without reading closes it (EPIPE).
            let toChild = stdin.fileHandleForWriting, fromChild = stdout.fileHandleForReading
            await Blocking.run {
                try? toChild.write(contentsOf: Data(input.utf8))
                try? toChild.close()
            }
            let output = await Blocking.run { fromChild.readDataToEndOfFile() }
            for await _ in exited {}
            return String(decoding: output, as: UTF8.self)
        }
        #expect(try await ask("protocol=https\nhost=gitlab.example.com\npath=team/x.git\n\n") == "username=oauth2\npassword=glov-secret:gl\n")
        #expect(try await ask("protocol=https\nhost=github.com\n\n") == "username=x-access-token\npassword=glov-secret:gh\n")
        #expect(try await ask("protocol=https\nhost=example.org\n\n") == "")
        #expect(try await ask("protocol=http\nhost=github.com\n\n") == "")
        #expect(try await ask("protocol=https\nhost=github.com\n\n", "store") == "")
    }

    @Test func remotesNameTheirRepositories() {
        let config = """
            [core]
            \trepositoryformatversion = 0
            [remote "origin"]
            \turl = git@github.com:Owner/App.git
            \tfetch = +refs/heads/*:refs/remotes/origin/*
            [remote "gl"]
            \turl = "https://user@gitlab.example.com:443/team/sub/proj.git"
            [branch "main"]
            \turl = https://not.a/remote
            """
        let urls = GitSignIn.remoteURLs(gitConfig: config)
        #expect(urls == ["git@github.com:Owner/App.git", "https://user@gitlab.example.com:443/team/sub/proj.git"])
        #expect(urls.compactMap(GitSignIn.repo(remote:)).map(\.text) == ["github.com/owner/app", "gitlab.example.com/team/sub/proj"])
        #expect(GitSignIn.repo(remote: "ssh://git@github.com/o/r")?.text == "github.com/o/r")
        for bad in ["/srv/git/app.git", "../app", "file:///srv/app.git", "github.com", "https://github.com/onlyowner", "https://github.com/owner/*", "git@gitlab.example.com:group/*",
                    "https://github.com/owner/../other"] {
            #expect(GitSignIn.repo(remote: bad) == nil, "\(bad)")
        }
    }
}
