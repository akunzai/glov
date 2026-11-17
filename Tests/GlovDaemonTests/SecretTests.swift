import Foundation
import GlovCore
import Synchronization
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct SecretTests {
    let management = EgressManagementTests()

    @Test func secretsAreDefinedByNameAndNoMethodReturnsAValue() async throws {
        let home = try management.makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let values = MemoryValues()
        let d = Dispatcher(home: home, secrets: SecretBroker(home: home, values: values))

        let set = try await management.call(d, Method.secretSet, Params.Secret(name: "work", hosts: ["API.example.com", "*.example.org", "api.example.com"], value: "tok-REAL"))
        // Too short for its ending to be shown: the length alone.
        #expect(try set.result?.decode(Results.Secret.self) == Results.Secret(name: "work", source: "keychain", hosts: ["api.example.com", "*.example.org"], length: 8))
        let long = try await management.call(d, Method.secretSet, Params.Secret(name: "long", hosts: ["x.example.com"], value: "REALREALREALREAL-wxyz"))
        #expect(try long.result?.decode(Results.Secret.self).ending == "wxyz")
        #expect(values.get("work") == "tok-REAL")
        let source = try await management.call(d, Method.secretSource, Params.Secret(name: "gh", hosts: ["github.com"], command: ["/usr/bin/true", "x"]))
        #expect(try source.result?.decode(Results.Secret.self) == Results.Secret(name: "gh", source: "command", command: ["/usr/bin/true", "x"], hosts: ["github.com"]))

        let listed = try await management.call(d, Method.secretList, Params.Secret())
        let secrets = try #require(try listed.result?.decode(Results.Secrets.self).secrets)
        #expect(secrets.map(\.name) == ["gh", "long", "work"])
        // What may be shown of a value comes from secrets.json, so listing reads no value.
        #expect(secrets.map(\.length) == [nil, 21, 8] && secrets.map(\.ending) == [nil, "wxyz", nil])
        #expect(listed.result.map { "\($0)" }?.contains("REAL") == false)
        let file = try String(contentsOfFile: home.path + "/secrets.json", encoding: .utf8)
        #expect(!file.contains("REAL"))
        #expect((try FileManager.default.attributesOfItem(atPath: home.path + "/secrets.json")[.posixPermissions] as? Int) == 0o600)

        // Replacing a Keychain secret with a command drops the stored value.
        _ = try await management.call(d, Method.secretSource, Params.Secret(name: "work", hosts: ["api.example.com"], command: ["/usr/bin/true"]))
        #expect(values.get("work") == nil)

        func message(_ method: String, _ p: Params.Secret) async throws -> String? { try await management.call(d, method, p).error?.message }
        #expect(try await message(Method.secretSet, .init(name: "a b", hosts: ["x.com"], value: "v")) == "secret name a b: use letters, digits, '.', '_' or '-', at most 64")
        #expect(try await message(Method.secretSet, .init(name: "n", hosts: [], value: "v")) == "secret n: bind it to at least one host with --host")
        #expect(try await message(Method.secretSet, .init(name: "n", hosts: ["not a host"], value: "v"))?.contains("not a valid egress entry") == true)
        #expect(try await message(Method.secretSet, .init(name: "n", hosts: ["x.com"], value: "")) == "secret n: the value is empty")
        #expect(try await message(Method.secretSource, .init(name: "n", hosts: ["x.com"], command: ["gh"])) == "secret n: give the command with an absolute path")
        #expect(try await message(Method.secretRemove, .init(name: "none")) == "no secret named none")
    }

    @Test func gitSecretsNameTheirForgeAndVMsKeepTheirRepositories() async throws {
        let home = try management.makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let d = Dispatcher(home: home, secrets: SecretBroker(home: home, values: MemoryValues()))
        let source = try await management.call(d, Method.secretSource, Params.Secret(name: "gh", hosts: ["github.com"], command: ["/usr/bin/true"], forge: "github"))
        #expect(try source.result?.decode(Results.Secret.self).forge == "github")
        #expect(try await management.call(d, Method.secretSet, Params.Secret(name: "x", hosts: ["x.com"], value: "v", forge: "svn")).error?.message == "forge svn: use github or gitlab")

        func repos(_ p: Params.SecretRepos) async throws -> Results.SecretRepos? { try await management.call(d, Method.secretRepos, p).result?.decode(Results.SecretRepos.self) }
        #expect(try await repos(.init(vm: "box")) == Results.SecretRepos(vm: "box", repos: []))
        let added = try await repos(.init(vm: "bo", add: ["https://GitHub.com/Owner/App.git", "gitlab.example.com/team/*", "github.com/owner/app"]))
        #expect(added == Results.SecretRepos(vm: "box", repos: ["github.com/owner/app", "gitlab.example.com/team/*"]))
        #expect(try home.loadConfig("box").egress?.repos == ["github.com/owner/app", "gitlab.example.com/team/*"])
        #expect(try await repos(.init(vm: "box", remove: ["github.com/owner/app"]))?.repos == ["gitlab.example.com/team/*"])
        #expect(try await management.call(d, Method.secretRepos, Params.SecretRepos(vm: "box", remove: ["github.com/owner/app"])).error?.message == "VM box has no repository github.com/owner/app")
        #expect(try await management.call(d, Method.secretRepos, Params.SecretRepos(vm: "box", add: ["github.com/owner"])).error?.message.contains("<host>/<owner>/<repo>") == true)
        #expect(try await management.call(d, Method.secretRepos, Params.SecretRepos(vm: "plain", add: ["github.com/a/b"])).error?.message.contains("not isolated") == true)
    }

    @Test func gitHubNodeAnswersTieEachNodeToItsRepositoryOrToNone() throws {
        let answer = Data("""
            {"data":{"nodes":[{"__typename":"Repository","nameWithOwner":"Owner/App"},{"__typename":"PullRequest","repository":{"nameWithOwner":"owner/app"}},
            {"__typename":"User"}]}}
            """.utf8)
        let nodes = try GitHubNodes.parse(answer, ids: ["R", "PR", "U"])
        let want: [String: String?] = ["R": "owner/app", "PR": "owner/app", "U": nil]
        #expect(nodes == want)
        #expect(throws: GitHubNodes.Failure.self) { try GitHubNodes.parse(Data(#"{"data":{"nodes":[null]}}"#.utf8), ids: ["gone"]) }
        #expect(throws: GitHubNodes.Failure.self) { try GitHubNodes.parse(Data(#"{"data":{"nodes":[{"__typename":"Gist"}]}}"#.utf8), ids: ["G"]) }
        #expect(GitHubNodes.endpoint("api.github.com")?.absoluteString == "https://api.github.com/graphql")
        #expect(GitHubNodes.endpoint("ghe.example.com")?.absoluteString == "https://ghe.example.com/api/graphql")
    }

    @Test func grantsAreKeptPerIsolatedVMAndRemovingASecretRevokesIt() async throws {
        let home = try management.makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let d = Dispatcher(home: home, secrets: SecretBroker(home: home, values: MemoryValues()))
        for name in ["gh", "npm"] {
            _ = try await management.call(d, Method.secretSet, Params.Secret(name: name, hosts: ["\(name).example.com"], value: "v"))
        }

        let granted = try await management.call(d, Method.secretGrant, Params.SecretGrant(vm: "bo", names: ["gh", "npm", "gh"]))
        #expect(try granted.result?.decode(Results.SecretGrants.self) == Results.SecretGrants(vm: "box", names: ["gh", "npm"]))
        #expect(try home.loadConfig("box").egress?.secrets == ["gh", "npm"])
        #expect(try await management.call(d, Method.secretGrant, Params.SecretGrant(vm: "box", names: ["nope"])).error?.message
            == "no secret named nope: define it with glov secret set or glov secret source")
        #expect(try await management.call(d, Method.secretGrant, Params.SecretGrant(vm: "plain", names: ["gh"])).error?.message.contains("not isolated") == true)
        #expect(try await management.call(d, Method.secretGrant, Params.SecretGrant(vm: "box", names: [])).error?.code == RPCError.invalidParams)

        let revoked = try await management.call(d, Method.secretRevoke, Params.SecretGrant(vm: "box", names: ["npm"]))
        #expect(try revoked.result?.decode(Results.SecretGrants.self) == Results.SecretGrants(vm: "box", names: ["gh"]))
        #expect(try await management.call(d, Method.secretRevoke, Params.SecretGrant(vm: "box", names: ["npm"])).error?.message == "VM box was not granted npm")

        // A grant whose secret vanished from secrets.json behind glovd's back shows as missing.
        _ = try await management.call(d, Method.secretGrant, Params.SecretGrant(vm: "box", names: ["npm"]))
        let all = try JSONDecoder().decode([SecretDefinition].self, from: Data(contentsOf: URL(fileURLWithPath: home.path + "/secrets.json")))
        try JSONEncoder().encode(all.filter { $0.name != "npm" }).write(to: URL(fileURLWithPath: home.path + "/secrets.json"))
        let listed = try await management.call(d, Method.secretGrants, Params.SecretGrant(vm: "box"))
        #expect(try listed.result?.decode(Results.SecretGrants.self) == Results.SecretGrants(vm: "box", names: ["gh", "npm"], missing: ["npm"]))

        #expect(try await management.call(d, Method.secretRemove, Params.Secret(name: "gh")).error == nil)
        #expect(try home.loadConfig("box").egress?.secrets == ["npm"])
    }

    @Test func aCommandValueIsCachedForTheTTLAndErrorsNeverCarryItsOutput() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let calls = Mutex(0)
        let clock = Mutex(ContinuousClock.now)
        let broker = SecretBroker(
            home: home, values: MemoryValues(),
            runner: { argv, _ in
                let n = calls.withLock { $0 += 1; return $0 }
                if argv.last == "fail" { throw SecretBroker.CommandFailure(description: "\(argv[0]) exited with status 3") }
                return argv.last == "empty" ? "" : "token-\(n)"
            },
            now: { clock.withLock { $0 } })
        try await broker.source("gh", hosts: ["github.com"], command: ["/opt/gh", "auth", "token"])
        #expect(try await broker.value("gh") == "token-1")
        #expect(try await broker.value("gh") == "token-1")
        clock.withLock { $0 += SecretBroker.ttl + .seconds(1) }
        #expect(try await broker.value("gh") == "token-2")

        try await broker.source("bad", hosts: ["x.com"], command: ["/opt/x", "fail"])
        await #expect { try await broker.value("bad") } throws: { "\($0)".contains("secret bad: /opt/x exited with status 3") }
        try await broker.source("blank", hosts: ["x.com"], command: ["/opt/x", "empty"])
        await #expect { try await broker.value("blank") } throws: { "\($0)".contains("secret blank: /opt/x printed nothing") }
        try await broker.set("kc", hosts: ["x.com"], value: "v")
        #expect(try await broker.value("kc") == "v")
        await #expect { try await broker.value("none") } throws: { "\($0)".contains("no secret named none") }
    }

    @Test(.timeLimit(.minutes(1)))
    func theRunnerReturnsStdoutAndReportsOnlyTheExitStatus() async throws {
        #expect(try await SecretBroker.run(["/bin/echo", "hello"], timeout: .seconds(5)) == "hello")
        await #expect { try await SecretBroker.run(["/bin/sh", "-c", "echo LEAKED; echo LEAKED >&2; exit 3"], timeout: .seconds(5)) } throws: { error in
            "\(error)" == "/bin/sh exited with status 3"
        }
        await #expect { try await SecretBroker.run(["/bin/sleep", "30"], timeout: .milliseconds(200)) } throws: { "\($0)".contains("did not finish") }
        await #expect { try await SecretBroker.run(["/nonexistent"], timeout: .seconds(1)) } throws: { "\($0)" == "cannot run /nonexistent" }
    }

    @Test(.timeLimit(.minutes(1)))
    func glovSecretCommands() async throws {
        let home = try management.makeHome()
        let values = MemoryValues()
        let server = try DaemonServer(home: home, dispatcher: Dispatcher(home: home, secrets: SecretBroker(home: home, values: values)))
        server.start()
        defer { server.stop(); try? FileManager.default.removeItem(atPath: home.path) }

        let set = try await runglov(["secret", "set", "npm", "--host", "registry.npmjs.org"], home: home, stdin: " npm-REAL-0123456789\n")
        #expect(set.status == 0 && set.out == "stored secret npm for registry.npmjs.org (19 characters, ending …6789)\n", "\(set)")
        #expect(values.get("npm") == "npm-REAL-0123456789")
        // A bare command name is resolved on the caller's PATH.
        let source = try await runglov(["secret", "source", "gh", "--host", "github.com", "--", "true", "x"], home: home)
        #expect(source.status == 0 && source.out == "secret gh comes from /usr/bin/true for github.com\n", "\(source)")
        let list = try await runglov(["secret", "list"], home: home)
        #expect(list.out == "gh\tcommand: /usr/bin/true x\tgithub.com\nnpm\tkeychain\tregistry.npmjs.org\t19 characters, ending …6789\n" && !list.out.contains("REAL"))

        #expect(try await runglov(["secret", "grant", "box", "gh", "npm"], home: home).out == "gh\nnpm\n")
        #expect(try await runglov(["secret", "revoke", "box", "npm"], home: home).out == "gh\n")
        #expect(try await runglov(["secret", "grants", "box"], home: home).out == "gh\n")
        #expect(try await runglov(["secret", "remove", "gh"], home: home).out == "removed secret gh\n")
        #expect(try await runglov(["secret", "grants", "box"], home: home).out == "box: no secrets granted\n")
        #expect(try await runglov(["secret", "source", "x", "--host", "x.com"], home: home).status != 0)
    }
}

@Suite struct KeychainValuesTests {
    @Test func aValueIsStoredListedReadAndRemovedUnderItsOwnService() throws {
        let store = KeychainValues(service: "glov test \(UUID().uuidString)")
        defer { _ = try? store.remove("work") }
        #expect(try store.get("work") == nil && store.names().isEmpty)
        try store.set("work", "first")
        try store.set("work", "second")
        #expect(try store.get("work") == "second")
        #expect(try store.names() == ["work"])
        #expect(try store.remove("work"))
        #expect(try !store.remove("work"))
    }
}
