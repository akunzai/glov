import Foundation
import GlovCore
import NIOCore
import NIOSSL
import Synchronization
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// The egress proxy holding a VM's git requests to its repositories (`glov secret repos`), through a real TLS upstream.
@Suite struct RepoAllowlistTests {
    static let host = "api.github.com"

    final class Lines: Sendable { let list = Mutex<[String]>([]) }

    struct Fixture {
        let proxy: EgressProxy
        let ca: EgressCA
        let upstream: SecretSubstitutionTests.Upstream
        let lines: Lines
        let cleanup: @Sendable () -> Void

        func send(_ request: String) async throws -> String {
            try await SecretSubstitutionTests().exchange(proxy, host: RepoAllowlistTests.host, request, trust: ca)
        }

        /// The egress.log lines so far, once `count` are written.
        func log(_ count: Int) async throws -> [String] {
            for _ in 0..<100 where lines.list.withLock({ $0.count }) < count { try await Task.sleep(for: .milliseconds(20)) }
            return lines.list.withLock { $0 }
        }
    }

    /// A VM granted git secret `gh` (forge github) for api.github.com with `repos`; node IDs resolve through `nodes`.
    func fixture(repos: [String], nodes: [String: String?] = [:]) throws -> Fixture {
        let home = GlovHome(path: try shortTempDir()), upstreamHome = GlovHome(path: try shortTempDir())
        let upstream = try SecretSubstitutionTests.Upstream(home: upstreamHome, host: Self.host)
        let ca = try EgressCA.loadOrCreate(home: home)
        let tls = try EgressTLS(ca: ca, upstreamTrust: .certificates([try NIOSSLCertificate(bytes: Array(upstream.ca.certificatePEM.utf8), format: .pem)]))
        var definition = try SecretDefinition(name: "gh", source: .keychain, hosts: [Self.host])
        definition.forge = .github
        let gh = definition
        let lines = Lines()
        let port = upstream.port
        let proxy = EgressProxy(
            vm: "box", egress: { .init(allow: [Self.host], secrets: ["gh"], repos: repos) }, resolve: { _, _ in ["93.184.215.14"] },
            localAddresses: { [] }, connect: { _, _ in EgressNet.connect("127.0.0.1", port) }, log: { r in lines.list.withLock { $0.append(r.line) } },
            tls: tls, secretDefinitions: { [gh] }, secretValue: { _ in "tok-REAL" },
            resolveNodes: { host, token, ids in
                #expect(host == Self.host && token == "tok-REAL")
                return try ids.reduce(into: [String: String?]()) { out, id in
                    guard let repo = nodes[id] else { throw GitHubNodes.Failure("node \(id) does not exist") }
                    out[id] = repo
                }
            })
        return Fixture(proxy: proxy, ca: ca, upstream: upstream, lines: lines) {
            try? upstream.channel.close().wait()
            for h in [home, upstreamHome] { try? FileManager.default.removeItem(atPath: h.path) }
        }
    }

    func get(_ path: String, signedIn: Bool = true) -> String {
        "GET \(path) HTTP/1.1\r\nHost: \(Self.host)\r\n" + (signedIn ? "Authorization: token glov-secret:gh\r\n" : "") + "\r\n"
    }

    func graphQL(_ query: String, _ variables: [String: Any] = [:]) -> String {
        let body = String(decoding: try! JSONSerialization.data(withJSONObject: ["query": query, "variables": variables]), as: UTF8.self)
        return "POST /graphql HTTP/1.1\r\nHost: \(Self.host)\r\nAuthorization: bearer glov-secret:gh\r\nContent-Type: application/json\r\n"
            + "Content-Length: \(body.utf8.count)\r\n\r\n\(body)"
    }

    @Test(.timeLimit(.minutes(1)))
    func aSignedInRequestReachesOnlyTheVMsRepositories() async throws {
        let f = try fixture(repos: ["github.com/owner/app"])
        defer { f.cleanup() }
        let ok = try await f.send(get("/repos/Owner/App/pulls"))
        #expect(ok.hasPrefix("HTTP/1.1 200 OK"))
        #expect(f.upstream.heads.list.withLock { $0 }.first?.contains("Authorization: token tok-REAL") == true)

        let other = try await f.send(get("/repos/owner/secret-project/contents/README.md"))
        #expect(other.hasPrefix("HTTP/1.1 403 Forbidden") && other.contains("glov secret repos box add"), "\(other)")
        let write = try await f.send("POST /user/repos HTTP/1.1\r\nHost: \(Self.host)\r\nAuthorization: token glov-secret:gh\r\nContent-Length: 0\r\n\r\n")
        #expect(write.hasPrefix("HTTP/1.1 403 Forbidden"), "\(write)")
        #expect(f.upstream.heads.list.withLock { $0.count } == 1, "nothing refused went upstream")

        let log = try await f.log(3)
        #expect(log.contains { $0.contains("deny api.github.com:443 GET /repos/owner/secret-project/contents/README.md 403") && $0.hasSuffix("(repository github.com/owner/secret-project is not allowed)") }, "\(log)")
        #expect(!log.joined().contains("REAL"))
    }

    @Test(.timeLimit(.minutes(1)))
    func anAnonymousRequestReachesAnyRepository() async throws {
        // A public clone or package fetch carries no sign-in: it reaches what anyone on the internet can.
        let f = try fixture(repos: [])
        defer { f.cleanup() }
        let anonymous = try await f.send(get("/repos/someone/library/tarball/v1", signedIn: false))
        #expect(anonymous.hasPrefix("HTTP/1.1 200 OK"))
        let signedIn = try await f.send(get("/repos/someone/library/tarball/v1"))
        #expect(signedIn.hasPrefix("HTTP/1.1 403 Forbidden"))
    }

    @Test(.timeLimit(.minutes(1)))
    func graphQLIsHeldToTheRepositoriesItNamesAndItsNodeIDsResolveTo() async throws {
        let nodes: [String: String?] = ["R_app": "owner/app", "R_other": "owner/other", "U_me": nil]
        let f = try fixture(repos: ["github.com/owner/app"], nodes: nodes)
        defer { f.cleanup() }
        let create = graphQL(
            "mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { url } } }",
            ["input": ["repositoryId": "R_app", "assigneeIds": ["U_me"], "baseRefName": "main", "headRefName": "x", "title": "t"]])
        let ok = try await f.send(create)
        #expect(ok.hasPrefix("HTTP/1.1 200 OK"), "\(ok)")
        let sent = f.upstream.heads.list.withLock { $0 }.last ?? ""
        #expect(sent.contains("Authorization: bearer tok-REAL") && sent.contains("\"repositoryId\":\"R_app\""), "the body goes on as it came: \(sent)")

        let elsewhere = try await f.send(graphQL(
            "mutation($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { url } } }", ["input": ["repositoryId": "R_other"]]))
        #expect(elsewhere.hasPrefix("HTTP/1.1 403 Forbidden") && elsewhere.contains("github.com/owner/other"), "\(elsewhere)")
        let byName = try await f.send(graphQL("query($o: String!, $n: String!) { repository(owner: $o, name: $n) { id } }", ["o": "owner", "n": "other"]))
        #expect(byName.hasPrefix("HTTP/1.1 403 Forbidden"), "\(byName)")
        let unknown = try await f.send(graphQL("query { node(id: \"X_9\") { id } }"))
        #expect(unknown.hasPrefix("HTTP/1.1 403 Forbidden") && unknown.contains("node X_9"), "\(unknown)")
        let search = try await f.send(graphQL("query { search(query: \"secret\", type: REPOSITORY, first: 1) { repositoryCount } }"))
        #expect(search.hasPrefix("HTTP/1.1 403 Forbidden"), "\(search)")
        let orphan = try await f.send(graphQL("mutation($i: CreateRepositoryInput!) { createRepository(input: $i) { repository { url } } }", ["i": ["ownerId": "U_me", "name": "x"]]))
        #expect(orphan.hasPrefix("HTTP/1.1 403 Forbidden") && orphan.contains("names no allowed repository"), "\(orphan)")
        let viewer = try await f.send(graphQL("query { viewer { login } }"))
        #expect(viewer.hasPrefix("HTTP/1.1 200 OK"))
        #expect(f.upstream.heads.list.withLock { $0.count } == 2)
    }
}
