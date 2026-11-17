import Foundation

/// The code forge a git secret signs in to (`glov secret source --forge`), so the egress proxy can tell which
/// repository a request to its hosts names and hold it to the VM's repositories (`glov secret repos`).
public enum Forge: String, Codable, Sendable, CaseIterable {
    case github, gitlab
}

/// One entry of a VM's repositories: `<host>/<owner>/<repo>`, or `<host>/<group>/*` for every repository under a
/// group (GitLab subgroups included). Hosts and paths compare without case; a trailing `.git` is dropped.
public struct RepoPattern: Sendable, Equatable, Hashable {
    public let host: String
    /// `owner/repo` or `group/sub/repo`, or `group/*`.
    public let path: String

    public init?(_ text: String) {
        var t = text.trimmingCharacters(in: .whitespaces).lowercased()
        for scheme in ["https://", "http://"] where t.hasPrefix(scheme) { t.removeFirst(scheme.count) }
        while t.hasSuffix("/") { t.removeLast() }
        if t.hasSuffix(".git") { t.removeLast(4) }
        var parts = t.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard parts.count >= 3, let host = parts.first, EgressPolicy.canonical(host) == host, !host.hasPrefix("*") else { return nil }
        parts.removeFirst()
        let segment = Set("abcdefghijklmnopqrstuvwxyz0123456789._-")
        for (i, p) in parts.enumerated() {
            let wildcard = p == "*" && i == parts.count - 1 && i > 0
            guard wildcard || (!p.isEmpty && p != "." && p != ".." && p.allSatisfy(segment.contains)) else { return nil }
        }
        self.host = host
        path = parts.joined(separator: "/")
    }

    public var text: String { "\(host)/\(path)" }

    /// Whether `repo` (`owner/repo`, lower case) on `host` is this entry or, for `group/*`, under it.
    public func matches(host: String, repo: String) -> Bool {
        guard host.lowercased() == self.host else { return false }
        let repo = repo.lowercased()
        if path.hasSuffix("/*") { return repo.hasPrefix(String(path.dropLast())) }
        return repo == path
    }
}

/// What one request to a forge's host reaches, as far as its method and path tell (`RepoAccess.target`).
public enum RepoTarget: Sendable, Equatable {
    /// A repository's path under the forge (`owner/repo`, `group/sub/repo`), lower case.
    case repo(String)
    /// Nothing tied to one repository (`/user`, the API root).
    case none
    /// A GraphQL request: the repositories are in its body (`GraphQLRepos`).
    case graphQL
    /// A request whose repository cannot be told from it, refused: the reason.
    case unknown(String)
}

/// Tells which repository a request to a forge names, and whether a VM's repositories allow it. Pure, so every
/// path shape is tested; the egress proxy applies it to intercepted requests (docs/networking.md "Repositories").
public enum RepoAccess {
    /// The host repositories are named under for a request to `host`: GitHub serves its API and uploads from
    /// subdomains of github.com.
    public static func repoHost(_ forge: Forge, _ host: String) -> String {
        let host = host.lowercased()
        if forge == .github, ["api.github.com", "uploads.github.com"].contains(host) { return "github.com" }
        return host
    }

    /// The repository a request with `path` (no query; percent-encoding as sent) reaches.
    public static func target(_ forge: Forge, host: String, path: String) -> RepoTarget {
        let host = host.lowercased()
        switch forge {
        case .github:
            if host == "api.github.com" { return githubAPI(path) }
            if host == "uploads.github.com" {
                let t = githubAPI(path)
                return t == .graphQL ? .unknown("GraphQL is served by api.github.com") : t
            }
            if path == "/api/graphql" { return .graphQL }
            if path == "/api/v3" || path.hasPrefix("/api/v3/") { return githubAPI(String(path.dropFirst("/api/v3".count))) }
            return gitPath(path, nested: false)
        case .gitlab:
            if path == "/api/graphql" { return .unknown("GitLab GraphQL names projects in ways the proxy does not check; use the REST API") }
            if path == "/api/v4" || path.hasPrefix("/api/v4/") { return gitlabAPI(String(path.dropFirst("/api/v4".count))) }
            return gitPath(path, nested: true)
        }
    }

    /// Why a request is refused, or nil when the VM's repositories allow it. A request naming no repository may only read.
    public static func refusal(_ target: RepoTarget, method: String, host: String, allowed: [RepoPattern]) -> String? {
        switch target {
        case .repo(let repo):
            return allowed.contains { $0.matches(host: host, repo: repo) } ? nil : "repository \(host)/\(repo) is not allowed"
        case .none:
            return ["GET", "HEAD", "OPTIONS"].contains(method.uppercased()) ? nil : "a \(method.uppercased()) that names no allowed repository"
        case .graphQL:
            return nil
        case .unknown(let reason):
            return reason
        }
    }

    /// The decoded segments of `path`, or nil when one could name a different path once decoded or normalized
    /// upstream: `.` or `..`, an empty segment inside the path, a decoded `/`.
    static func segments(_ path: String) -> [String]? {
        var raw = path.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        if raw.first == "" { raw.removeFirst() }
        if raw.last == "" { raw.removeLast() }
        var out: [String] = []
        for r in raw {
            guard let d = r.removingPercentEncoding, !d.isEmpty, d != ".", d != "..", !d.contains("/"), !d.contains("\\") else { return nil }
            out.append(d)
        }
        return out
    }

    static let oddPath = RepoTarget.unknown("a path with an empty, dot or encoded-slash segment")

    /// GitHub API reads of no repository a signed-in request may make: the token's own user and the API's metadata.
    /// Everything else (`/user/repos`, `/issues`, `/notifications`, an organization's lists) spans repositories.
    static let githubFree: Set<[String]> = [[], ["user"], ["user", "emails"], ["rate_limit"], ["meta"], ["octocat"], ["zen"], ["versions"]]

    private static func githubAPI(_ path: String) -> RepoTarget {
        guard let s = segments(path) else { return oddPath }
        let lower = s.map { $0.lowercased() }
        if githubFree.contains(lower) || (lower.count == 2 && ["users", "orgs"].contains(lower[0])) { return .none }
        switch lower[0] {
        case "graphql" where s.count == 1: return .graphQL
        case "repos":
            guard s.count >= 3 else { return .unknown("a repository request without owner and name") }
            return .repo("\(lower[1])/\(lower[2])")
        case "repositories": return .unknown("a repository named by its number")
        case "search": return .unknown("search reaches every repository the token can read")
        default: return .unknown("an API path of no one repository: /\(s.joined(separator: "/"))")
        }
    }

    /// GitLab API reads of no project a signed-in request may make.
    static let gitlabFree: Set<[String]> = [[], ["user"], ["version"], ["metadata"], ["personal_access_tokens", "self"]]

    private static func gitlabAPI(_ path: String) -> RepoTarget {
        // A project's path is one segment with its slashes encoded (`group%2Fproject`), so split before decoding.
        var raw = path.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        if raw.first == "" { raw.removeFirst() }
        if raw.count >= 2, raw[0].lowercased() == "projects" {
            guard let id = raw[1].removingPercentEncoding, let parts = segments(id), parts.count >= 2,
                  segments("/" + raw.dropFirst(2).joined(separator: "/")) != nil
            else {
                if raw[1].allSatisfy(\.isNumber) { return .unknown("a project named by its number; name it by its path (group%2Fproject)") }
                return oddPath
            }
            return .repo(parts.joined(separator: "/").lowercased())
        }
        guard let s = segments(path) else { return oddPath }
        let lower = s.map { $0.lowercased() }
        if gitlabFree.contains(lower) || (lower.count == 2 && lower[0] == "users") { return .none }
        if lower.contains("search") { return .unknown("search reaches every project the token can read") }
        return .unknown("an API path of no one project: /\(s.joined(separator: "/"))")
    }

    /// A git smart HTTP or web path: `owner/repo` on GitHub; on GitLab the path up to `/-/`, `.git` or a git service
    /// endpoint, subgroups included. One segment names no repository.
    private static func gitPath(_ path: String, nested: Bool) -> RepoTarget {
        guard var s = segments(path) else { return oddPath }
        guard s.count >= 2 else { return .none }
        if !nested {
            return .repo(trimGit("\(s[0])/\(s[1])").lowercased())
        }
        if let dash = s.firstIndex(of: "-") { s = Array(s[..<dash]) }
        if let git = s.firstIndex(where: { $0.lowercased().hasSuffix(".git") }) { s = Array(s[...git]) }
        let endpoints: [[String]] = [["info", "refs"], ["git-upload-pack"], ["git-receive-pack"], ["info", "lfs"]]
        for e in endpoints {
            if let i = (0...max(0, s.count - e.count)).first(where: { i in i + e.count <= s.count && s[i..<(i + e.count)].map { $0.lowercased() } == e }), i > 0 {
                s = Array(s[..<i])
                break
            }
        }
        guard s.count >= 2 else { return .unknown("a path that names no project") }
        return .repo(trimGit(s.joined(separator: "/")).lowercased())
    }

    private static func trimGit(_ path: String) -> String { path.lowercased().hasSuffix(".git") ? String(path.dropLast(4)) : path }
}

/// Decides one GitHub GraphQL request (docs/networking.md "Repositories") by an allowlist over its parsed query, so
/// nothing the proxy does not understand reaches a repository outside the VM's list:
///
/// - A query's top-level fields are `repository(owner:, name:)`, `node(id:)` and `nodes(ids:)` (by ID, resolved through
///   the forge), `viewer`, `user` and `organization`, or fields of no repository (`rateLimit`, `meta`, introspection).
/// - Inside an allowed repository, fields that lead to other repositories (`forks`, `parent`, cross-references) are
///   refused, and fields that lead to a person (`author`, `assignees`, `owner`) switch to the person rules.
/// - Under a person (`viewer`, a user, an organization, a non-repository node), only fields without a selection set
///   (`login`, `name`) and connection plumbing (`nodes`, `edges`) are read: a user's repositories, issues or pull
///   requests span repositories the request does not name.
/// - Every top-level mutation field must name an allowed repository by node ID (`repositoryId`, `pullRequestId`, ...)
///   or `nameWithOwner`; its other IDs must be allowed repositories or people. Subscriptions are refused.
public enum GraphQLRepos {
    /// A parsed request: the node IDs to resolve before `refusal` can decide.
    public struct Request: @unchecked Sendable {
        let operations: [Operation]
        let fragments: [String: [Selection]]
        public let nodeIDs: Set<String>
    }

    /// A refusal's reason.
    public struct Refusal: Error, Equatable, Sendable {
        public var reason: String
        public init(_ reason: String) { self.reason = reason }
    }

    /// Top-level query fields that read nothing of a repository.
    static let free: Set<String> = ["__typename", "__schema", "__type", "rateLimit", "meta"]
    /// Top-level query fields about a person.
    static let people: Set<String> = ["viewer", "user", "organization"]
    /// Fields inside a repository that lead to another repository.
    static let elsewhere: Set<String> = ["forks", "canonical", "duplicate", "closer",
                                         "repositories", "repository", "search", "network", "dependencyGraphManifests",
                                         // issues and pull requests that may sit in other repositories
                                         "subIssues", "trackedIssues", "trackedInIssues", "blockedBy", "blocking", "closingIssuesReferences",
                                         "closedByPullRequestsReferences", "subject", "fromRepository", "commitRepository", "referencedSubjects"]
    /// Fields inside a repository that lead to a person or a project, read by the person rules.
    static let toPeople: Set<String> = ["owner", "author", "editor", "actor", "user", "assignees", "participants", "mentionableUsers",
                                        "assignableUsers", "collaborators", "stargazers", "watchers", "requestedReviewer", "mergedBy",
                                        "enqueuedBy", "organization", "viewer", "committer", "reactors", "suggestedReviewers", "latestReviews",
                                        "reviewRequests", "headRepositoryOwner", "lastEditedBy",
                                        // a project holds items of any repository: only its own fields are read
                                        "project", "projectV2", "projectsV2", "projects"]
    static let projects: Set<String> = ["project", "projectV2", "projectsV2", "projects"]
    /// Connection plumbing a person's leaf fields may sit under.
    static let plumbing: Set<String> = ["nodes", "edges", "node", "pageInfo"]
    /// Fields inside a repository that lead to another one gh reads the identity of: a fork's upstream, a pull
    /// request's head and base. (`Issue.parent`, a sub-issue's parent, is read the same way: its ID and URL only.)
    static let peers: Set<String> = ["parent", "source", "templateRepository", "headRepository", "baseRepository"]
    /// Fields of another repository's identity that may be opened under `peers`, and the leaves read there.
    static let peerFields: Set<String> = ["owner", "defaultBranchRef", "parent", "source"]
    /// Leaves of an issue or pull request (`Issue.parent` is read here too) that hold its content.
    static let peerContent: Set<String> = ["title", "body", "bodyText", "bodyHTML", "text"]

    public static func parse(_ body: Data) -> Result<Request, Refusal> {
        guard let json = try? JSONSerialization.jsonObject(with: body) as? [String: Any], let query = json["query"] as? String else {
            return .failure(Refusal("a GraphQL request that is not one JSON object with a query"))
        }
        let variables = json["variables"] as? [String: Any] ?? [:]
        guard let tokens = Token.scan(query), var parser = Optional(Parser(tokens: tokens)), let document = parser.document() else {
            return .failure(Refusal("a GraphQL query that does not parse"))
        }
        var operations = document.operations
        if let name = json["operationName"] as? String, !name.isEmpty {
            operations = operations.filter { $0.name == name }
            guard !operations.isEmpty else { return .failure(Refusal("no GraphQL operation named \(name)")) }
        }
        // A variable the JSON leaves out takes its definition's default, as the server does, else null: an absent
        // argument names no repository, which the checks read as nothing allowed.
        for i in operations.indices {
            var values = variables
            for (name, value) in operations[i].defaults where values[name] == nil { values[name] = resolve(value, [:]) }
            operations[i].variables = values
        }
        var ids = Set<String>()
        for op in operations {
            for field in fields(op.selections, document.fragments) {
                let args: [String: Any] = field.arguments.compactMapValues { resolve($0, variables) }
                if op.kind == .mutation {
                    for (key, v) in args { collectIDs(key, v, into: &ids) }
                } else if field.name == "node" || field.name == "nodes" {
                    ids.formUnion(strings(args[field.name == "node" ? "id" : "ids"]))
                }
            }
        }
        return .success(Request(operations: operations, fragments: document.fragments, nodeIDs: ids))
    }

    /// Why the request is refused, or nil when it reaches only `allowed` repositories. `nodes` maps each of its
    /// `nodeIDs` to its repository (`owner/name`, lower case), or to nil for a person; an ID missing from it is refused.
    public static func refusal(_ request: Request, host: String, nodes: [String: String?], allowed: (String) -> Bool) -> String? {
        func repoOf(_ id: String) -> Result<String?, Refusal> {
            guard let known = nodes[id] else { return .failure(Refusal("node \(id) is of no repository the proxy can tell")) }
            if let repo = known, !allowed(repo) { return .failure(Refusal("repository \(host)/\(repo) is not allowed")) }
            return .success(known)
        }
        for op in request.operations {
            let variables = op.variables
            for field in fields(op.selections, request.fragments) {
                let args: [String: Any] = field.arguments.compactMapValues { resolve($0, variables) }
                switch op.kind {
                case .subscription:
                    return "GraphQL subscriptions are not checked"
                case .mutation:
                    var ids = Set<String>(), named = false
                    for (key, v) in args { collectIDs(key, v, into: &ids) }
                    for id in ids.sorted() {
                        switch repoOf(id) {
                        case .failure(let r): return r.reason
                        case .success(let repo): if repo != nil { named = true }
                        }
                    }
                    for name in nameWithOwner(args) {
                        guard allowed(name) else { return "repository \(host)/\(name) is not allowed" }
                        named = true
                    }
                    guard named else { return "GraphQL mutation \(field.name) names no allowed repository" }
                    if let r = walk(field.selections, .repo, request.fragments) { return r }
                case .query:
                    if free.contains(field.name) { continue }
                    if people.contains(field.name) {
                        if let r = walk(field.selections, .person, request.fragments) { return r }
                        continue
                    }
                    if field.name == "repository" {
                        guard let owner = strings(args["owner"]).first, let name = strings(args["name"]).first else {
                            return "GraphQL repository(...) without an owner and name the proxy can read"
                        }
                        let repo = "\(owner)/\(name)".lowercased()
                        guard allowed(repo) else { return "repository \(host)/\(repo) is not allowed" }
                        if let r = walk(field.selections, .repo, request.fragments) { return r }
                        continue
                    }
                    if field.name == "node" || field.name == "nodes" {
                        let ids = strings(args[field.name == "node" ? "id" : "ids"])
                        guard !ids.isEmpty else { return "GraphQL \(field.name) without IDs the proxy can read" }
                        var people = false
                        for id in ids {
                            switch repoOf(id) {
                            case .failure(let r): return r.reason
                            case .success(let repo): if repo == nil { people = true }
                            }
                        }
                        if let r = walk(field.selections, people ? .person : .repo, request.fragments) { return r }
                        continue
                    }
                    return "GraphQL field \(field.name) is not one the proxy checks"
                }
            }
        }
        return nil
    }

    /// `peer`: a repository reached from an allowed one (a fork's parent, a pull request's head): its identity only
    /// (`name`, `owner { login }`, `defaultBranchRef { name }`), so gh can tell a fork from its upstream.
    enum Scope { case repo, person, peer }

    /// The first refusal under `selections` read in `scope`.
    private static func walk(_ selections: [Selection], _ scope: Scope, _ fragments: [String: [Selection]], depth: Int = 0) -> String? {
        guard depth < 64 else { return "a GraphQL query nested too deep" }
        for field in fields(selections, fragments) {
            switch scope {
            case .repo:
                if peers.contains(field.name) {
                    if let r = walk(field.selections, .peer, fragments, depth: depth + 1) { return r }
                } else if elsewhere.contains(field.name) {
                    return "GraphQL field \(field.name) leads outside the allowed repository"
                } else if toPeople.contains(field.name) {
                    if let r = walk(field.selections, .person, fragments, depth: depth + 1) { return r }
                } else if let r = walk(field.selections, .repo, fragments, depth: depth + 1) {
                    return r
                }
            case .peer:
                guard field.selections.isEmpty && !peerContent.contains(field.name) || plumbing.contains(field.name) || peerFields.contains(field.name) else {
                    return "GraphQL field \(field.name) reads more of another repository than its name"
                }
                if let r = walk(field.selections, field.name == "owner" ? .person : .peer, fragments, depth: depth + 1) { return r }
            case .person:
                // A person's leaves, plumbing, and other people (a review request's `requestedReviewer`).
                guard field.selections.isEmpty || plumbing.contains(field.name) || toPeople.contains(field.name) && !projects.contains(field.name) else {
                    return "GraphQL field \(field.name) under a person reaches repositories the request does not name"
                }
                if let r = walk(field.selections, .person, fragments, depth: depth + 1) { return r }
            }
        }
        return nil
    }

    /// The fields of `selections`, inline fragments and fragment spreads opened. A spread of an unknown fragment, or
    /// of one inside itself, opens to nothing: the server refuses such a document whole.
    static func fields(_ selections: [Selection], _ fragments: [String: [Selection]], seen: Set<String> = []) -> [Field] {
        selections.flatMap { s -> [Field] in
            switch s {
            case .field(let f): return [f]
            case .inline(let inner): return fields(inner, fragments, seen: seen)
            case .spread(let name):
                guard !seen.contains(name), let inner = fragments[name] else { return [] }
                return fields(inner, fragments, seen: seen.union([name]))
            }
        }
    }

    static func isID(_ key: String) -> Bool {
        guard key != "clientMutationId" else { return false }
        return key == "id" || key == "ids" || key.hasSuffix("Id") || key.hasSuffix("ID") || key.hasSuffix("Ids") || key.hasSuffix("IDs")
    }

    /// Variables replaced by their JSON values.
    static func resolve(_ v: Value, _ variables: [String: Any]) -> Any? {
        switch v {
        case .string(let s): return s
        case .variable(let n): return variables[n]
        case .list(let items): return items.map { resolve($0, variables) as Any }
        case .object(let fields): return fields.mapValues { resolve($0, variables) as Any }
        case .other: return nil
        }
    }

    static func strings(_ v: Any?) -> [String] {
        if let s = v as? String { return [s] }
        if let a = v as? [Any] { return a.flatMap { strings($0) } }
        return []
    }

    private static func collectIDs(_ key: String, _ v: Any?, into ids: inout Set<String>) {
        if isID(key) { ids.formUnion(strings(v)) }
        if let dict = v as? [String: Any] { for (k, inner) in dict { collectIDs(k, inner, into: &ids) } }
        if let array = v as? [Any] { for inner in array { collectIDs(key, inner, into: &ids) } }
    }

    /// `owner/name` strings a mutation passes as `*NameWithOwner` or `*nameWithOwner`, at any depth.
    private static func nameWithOwner(_ args: [String: Any]) -> [String] {
        var out: [String] = []
        func visit(_ key: String, _ v: Any?) {
            if key.lowercased().hasSuffix("namewithowner") { out += strings(v).map { $0.lowercased() } }
            if let dict = v as? [String: Any] { for (k, inner) in dict { visit(k, inner) } }
            if let array = v as? [Any] { for inner in array { visit(key, inner) } }
        }
        for (k, v) in args { visit(k, v) }
        return out
    }

    enum Value: Equatable {
        case string(String), variable(String), list([Value]), object([String: Value]), other
    }

    struct Field: Equatable {
        var name: String
        var arguments: [String: Value]
        var selections: [Selection]
    }

    indirect enum Selection: Equatable {
        case field(Field), inline([Selection]), spread(String)
    }

    struct Operation: @unchecked Sendable {
        enum Kind { case query, mutation, subscription }
        var kind: Kind
        var name: String?
        var selections: [Selection]
        /// Default values of the variable definitions.
        var defaults: [String: Value] = [:]
        var variables: [String: Any] = [:]
    }

    struct Document {
        var operations: [Operation] = []
        var fragments: [String: [Selection]] = [:]
    }

    /// A recursive-descent reader of GraphQL's executable documents, over `Token`s; nil anywhere it does not follow
    /// the grammar, so the proxy never decides on a query it read differently from the server.
    struct Parser {
        let tokens: [Token]
        var i = 0

        init(tokens: [Token]) { self.tokens = tokens }

        mutating func document() -> Document? {
            var doc = Document()
            while i < tokens.count {
                if tokens[i] == .punct("{") {
                    guard let sel = selectionSet() else { return nil }
                    doc.operations.append(.init(kind: .query, selections: sel))
                    continue
                }
                guard case .name(let keyword) = tokens[i] else { return nil }
                i += 1
                if keyword == "fragment" {
                    guard case .name(let name)? = peek(), name != "on" else { return nil }
                    i += 1
                    guard peek() == .name("on") else { return nil }
                    i += 1
                    guard case .name? = peek() else { return nil }
                    i += 1
                    guard directives(), let sel = selectionSet(), doc.fragments[name] == nil else { return nil }
                    doc.fragments[name] = sel
                    continue
                }
                let kind: Operation.Kind
                switch keyword {
                case "query": kind = .query
                case "mutation": kind = .mutation
                case "subscription": kind = .subscription
                default: return nil
                }
                var name: String?
                if case .name(let n)? = peek() { name = n; i += 1 }
                var defaults: [String: Value] = [:]
                if peek() == .punct("(") { guard let d = variableDefinitions() else { return nil }; defaults = d }
                guard directives(), let sel = selectionSet() else { return nil }
                doc.operations.append(.init(kind: kind, name: name, selections: sel, defaults: defaults))
            }
            return doc.operations.isEmpty ? nil : doc
        }

        func peek() -> Token? { i < tokens.count ? tokens[i] : nil }

        /// `($name: Type = default @directive ...)`: the defaults given.
        mutating func variableDefinitions() -> [String: Value]? {
            i += 1
            var defaults: [String: Value] = [:]
            while let t = peek(), t != .punct(")") {
                guard t == .punct("$"), i + 2 < tokens.count, case .name(let name) = tokens[i + 1], tokens[i + 2] == .punct(":") else { return nil }
                i += 3
                guard type() else { return nil }
                if peek() == .punct("=") {
                    i += 1
                    guard let v = value() else { return nil }
                    defaults[name] = v
                }
                guard directives() else { return nil }
            }
            guard peek() == .punct(")") else { return nil }
            i += 1
            return defaults
        }

        /// `Name`, `[Type]`, each optionally `!`.
        mutating func type(depth: Int = 0) -> Bool {
            guard depth < 16 else { return false }
            if peek() == .punct("[") {
                i += 1
                guard type(depth: depth + 1), peek() == .punct("]") else { return false }
                i += 1
            } else {
                guard case .name? = peek() else { return false }
                i += 1
            }
            if peek() == .punct("!") { i += 1 }
            return true
        }

        mutating func selectionSet() -> [Selection]? {
            guard peek() == .punct("{") else { return nil }
            i += 1
            var out: [Selection] = []
            while let t = peek(), t != .punct("}") {
                if t == .punct(".") {
                    i += 1
                    if peek() == .name("on") {
                        i += 1
                        guard case .name? = peek() else { return nil }
                        i += 1
                        guard directives(), let inner = selectionSet() else { return nil }
                        out.append(.inline(inner))
                    } else if peek() == .punct("@") || peek() == .punct("{") {
                        guard directives(), let inner = selectionSet() else { return nil }
                        out.append(.inline(inner))
                    } else if case .name(let n)? = peek() {
                        i += 1
                        guard directives() else { return nil }
                        out.append(.spread(n))
                    } else {
                        return nil
                    }
                    continue
                }
                guard case .name(var name) = t else { return nil }
                i += 1
                if peek() == .punct(":") {
                    i += 1
                    guard case .name(let real)? = peek() else { return nil }
                    name = real
                    i += 1
                }
                var args: [String: Value] = [:]
                if peek() == .punct("(") { guard let a = arguments() else { return nil }; args = a }
                guard directives() else { return nil }
                var children: [Selection] = []
                if peek() == .punct("{") { guard let c = selectionSet() else { return nil }; children = c }
                out.append(.field(Field(name: name, arguments: args, selections: children)))
            }
            guard peek() == .punct("}") else { return nil }
            i += 1
            return out
        }

        mutating func arguments() -> [String: Value]? {
            i += 1
            var args: [String: Value] = [:]
            while let t = peek(), t != .punct(")") {
                guard case .name(let key) = t, i + 1 < tokens.count, tokens[i + 1] == .punct(":") else { return nil }
                i += 2
                guard let v = value() else { return nil }
                args[key] = v
            }
            guard peek() == .punct(")") else { return nil }
            i += 1
            return args
        }

        mutating func value() -> Value? {
            guard let t = peek() else { return nil }
            switch t {
            case .string(let s): i += 1; return .string(s)
            case .punct("$"):
                guard i + 1 < tokens.count, case .name(let n) = tokens[i + 1] else { return nil }
                i += 2
                return .variable(n)
            case .punct("["):
                i += 1
                var items: [Value] = []
                while let t = peek(), t != .punct("]") { guard let v = value() else { return nil }; items.append(v) }
                guard peek() == .punct("]") else { return nil }
                i += 1
                return .list(items)
            case .punct("{"):
                i += 1
                var fields: [String: Value] = [:]
                while let t = peek(), t != .punct("}") {
                    guard case .name(let key) = t, i + 1 < tokens.count, tokens[i + 1] == .punct(":") else { return nil }
                    i += 2
                    guard let v = value() else { return nil }
                    fields[key] = v
                }
                guard peek() == .punct("}") else { return nil }
                i += 1
                return .object(fields)
            case .name, .number: i += 1; return .other
            case .punct: return nil
            }
        }

        mutating func directives() -> Bool {
            while peek() == .punct("@") {
                i += 1
                guard case .name? = peek() else { return false }
                i += 1
                if peek() == .punct("(") { guard arguments() != nil else { return false } }
            }
            return true
        }
    }

    enum Token: Equatable {
        case name(String), string(String), number, punct(Character)

        /// GraphQL's lexical tokens, without comments and commas; nil for a query that does not lex. It reads Unicode scalars,
        /// not `Character`s: CR LF is one `Character`, equal to neither "\n" nor "\r", so a comment would run past it.
        static func scan(_ text: String) -> [Token]? {
            var tokens: [Token] = []
            let chars = Array(text.unicodeScalars)
            func string(_ scalars: ArraySlice<Unicode.Scalar>) -> String { String(String.UnicodeScalarView(scalars)) }
            func isLetter(_ c: Unicode.Scalar) -> Bool { c.properties.isAlphabetic }
            func isNumber(_ c: Unicode.Scalar) -> Bool { c.properties.numericType != nil }
            var i = 0
            while i < chars.count {
                let c = chars[i]
                if c == "#" { while i < chars.count, chars[i] != "\n", chars[i] != "\r" { i += 1 }; continue }
                if c.properties.isWhitespace || c == "," || c == "\u{FEFF}" { i += 1; continue }
                if c == "\"" {
                    if i + 2 < chars.count, chars[i + 1] == "\"", chars[i + 2] == "\"" {
                        var j = i + 3, s = ""
                        while j + 2 < chars.count, !(chars[j] == "\"" && chars[j + 1] == "\"" && chars[j + 2] == "\"") {
                            if chars[j] == "\\", j + 3 < chars.count, chars[j + 1] == "\"", chars[j + 2] == "\"", chars[j + 3] == "\"" { s += "\"\"\""; j += 4; continue }
                            s.unicodeScalars.append(chars[j]); j += 1
                        }
                        guard j + 2 < chars.count else { return nil }
                        tokens.append(.string(s.trimmingCharacters(in: .whitespacesAndNewlines)))
                        i = j + 3
                        continue
                    }
                    var j = i + 1, s = ""
                    while j < chars.count, chars[j] != "\"" {
                        if chars[j] == "\n" || chars[j] == "\r" { return nil }
                        if chars[j] == "\\" {
                            guard j + 1 < chars.count else { return nil }
                            let e = chars[j + 1]
                            switch e {
                            case "n": s += "\n"
                            case "t": s += "\t"
                            case "r": s += "\r"
                            case "b": s += "\u{8}"
                            case "f": s += "\u{C}"
                            case "u":
                                guard j + 5 < chars.count, let code = UInt32(string(chars[(j + 2)...(j + 5)]), radix: 16), let scalar = Unicode.Scalar(code) else { return nil }
                                s.unicodeScalars.append(scalar)
                                j += 4
                            default: s.unicodeScalars.append(e)
                            }
                            j += 2
                            continue
                        }
                        s.unicodeScalars.append(chars[j]); j += 1
                    }
                    guard j < chars.count else { return nil }
                    tokens.append(.string(s))
                    i = j + 1
                    continue
                }
                if isLetter(c) || c == "_" {
                    var j = i
                    while j < chars.count, isLetter(chars[j]) || isNumber(chars[j]) || chars[j] == "_" { j += 1 }
                    tokens.append(.name(string(chars[i..<j])))
                    i = j
                    continue
                }
                if isNumber(c) || c == "-" {
                    var j = i + 1
                    while j < chars.count, isNumber(chars[j]) || chars[j] == "." || chars[j] == "e" || chars[j] == "E" || chars[j] == "+" || chars[j] == "-" { j += 1 }
                    tokens.append(.number)
                    i = j
                    continue
                }
                if c == ".", i + 2 < chars.count, chars[i + 1] == ".", chars[i + 2] == "." { tokens.append(.punct(".")); i += 3; continue }
                guard "!$&()[]{}:=@|".unicodeScalars.contains(c) else { return nil }
                tokens.append(.punct(Character(c)))
                i += 1
            }
            return tokens
        }
    }
}
