import Foundation
import Testing

@testable import GlovCore

/// The shared repository cases (`testdata/egress/repos.json`), which the Windows edition's tests read too.
@Suite struct RepoAccessTests {
    let fixture: [String: Any] = {
        let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let data = try! Data(contentsOf: root.appendingPathComponent("testdata/egress/repos.json"))
        return try! JSONSerialization.jsonObject(with: data) as! [String: Any]
    }()

    func cases(_ key: String, in object: [String: Any]? = nil) -> [[String: Any]] { (object ?? fixture)[key] as! [[String: Any]] }

    /// A JSON value that may be null, as Swift reads it.
    func optional(_ v: Any?) -> String? { v as? String }

    /// A target as the fixture writes it.
    func describe(_ target: RepoTarget) -> String {
        switch target {
        case .repo(let repo): "repo:\(repo)"
        case .none: "none"
        case .graphQL: "graphQL"
        case .unknown: "unknown"
        }
    }

    @Test func patternsNormalizeAndRefuseWhatIsNotARepository() {
        for c in cases("patterns") {
            #expect(RepoPattern(c["text"] as! String)?.text == c["pattern"] as? String, "\(c["text"]!)")
        }
        for bad in fixture["invalidPatterns"] as! [String] { #expect(RepoPattern(bad) == nil, "\(bad)") }
        for c in cases("matches") {
            let pattern = RepoPattern(c["pattern"] as! String)!
            #expect(pattern.matches(host: c["host"] as! String, repo: c["repo"] as! String) == c["matches"] as! Bool, "\(c)")
        }
        for c in cases("repoHosts") {
            #expect(RepoAccess.repoHost(Forge(rawValue: c["forge"] as! String)!, c["host"] as! String) == c["repoHost"] as! String, "\(c)")
        }
    }

    @Test func pathsNameTheirRepository() {
        let targets = cases("targets")
        #expect(!targets.isEmpty)
        for c in targets {
            let target = RepoAccess.target(Forge(rawValue: c["forge"] as! String)!, host: c["host"] as! String, path: c["path"] as! String)
            #expect(describe(target) == c["target"] as! String, "\(c["host"]!)\(c["path"]!)")
        }
    }

    @Test func onlyAllowedRepositoriesAndReadsOfNoRepositoryPass() {
        let refusals = fixture["refusals"] as! [String: Any]
        let allowed = (refusals["allowed"] as! [String]).map { RepoPattern($0)! }
        for c in cases("cases", in: refusals) {
            let text = c["target"] as! String
            let target: RepoTarget = switch text {
            case "none": .none
            case "graphQL": .graphQL
            default: .repo(String(text.dropFirst("repo:".count)))
            }
            #expect(RepoAccess.refusal(target, method: c["method"] as! String, host: c["host"] as! String, allowed: allowed) == optional(c["refusal"]),
                    "\(c["method"]!) \(text) on \(c["host"]!)")
        }
        #expect(RepoAccess.refusal(.unknown("why"), method: "GET", host: "github.com", allowed: allowed) == "why")
    }

    @Test func graphQLRequestsReachOnlyAllowedRepositories() throws {
        let g = fixture["graphQL"] as! [String: Any]
        let host = g["host"] as! String, allowed = g["allowed"] as! [String]
        let nodes = (g["nodes"] as! [String: Any]).mapValues { optional($0) }
        func body(_ c: [String: Any]) -> Data {
            try! JSONSerialization.data(withJSONObject: ["query": c["query"]!, "variables": c["variables"] ?? [:]])
        }
        /// Why the request is refused, or nil; a parse failure is a refusal too.
        func refusal(_ c: [String: Any]) -> String? {
            switch GraphQLRepos.parse(body(c)) {
            case .failure(let r): r.reason
            case .success(let request): GraphQLRepos.refusal(request, host: host, nodes: nodes) { allowed.contains($0) }
            }
        }
        for c in cases("allowedRequests", in: g) {
            let why = refusal(c)
            #expect(why == nil, "refused (\(why ?? "")): \(c["query"]!)")
        }
        for c in cases("refusedRequests", in: g) {
            let why = refusal(c)
            #expect(why != nil, "allowed: \(c["query"]!)")
            if let want = optional(c["refusal"]) { #expect(why == want, "\(c["query"]!)") }
        }
        for c in cases("refusedBodies", in: g) {
            if case .failure(let r) = GraphQLRepos.parse(Data((c["body"] as! String).utf8)) {
                #expect(r.reason == c["refusal"] as! String, "\(c["body"]!)")
            } else {
                Issue.record("allowed: \(c["body"]!)")
            }
        }
        for c in cases("nodeIDs", in: g) {
            #expect(try GraphQLRepos.parse(body(c)).get().nodeIDs == Set(c["nodeIDs"] as! [String]), "\(c["query"]!)")
        }
    }
}
