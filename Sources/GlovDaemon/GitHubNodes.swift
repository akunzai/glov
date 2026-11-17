import Foundation
import Synchronization

/// Resolves the GitHub node IDs a guest's GraphQL request passes to the repository each belongs to, by asking the
/// forge itself with the VM's own git secret, so the egress proxy can hold a mutation by ID to the VM's repositories.
/// Answers are kept for the daemon's life: a node never moves to another repository... except a transferred
/// repository, whose new owner then has to be allowed too, which errs toward refusing.
enum GitHubNodes {
    /// Node types that belong to no repository and may be named freely: an assignee, a reviewer, an owner.
    static let unowned: Set<String> = ["User", "Organization", "Bot", "Team", "Mannequin"]

    static let query = """
        query($ids: [ID!]!) { nodes(ids: $ids) { __typename \
        ... on Repository { nameWithOwner } \
        ... on RepositoryNode { repository { nameWithOwner } } \
        ... on Ref { repository { nameWithOwner } } \
        ... on Commit { repository { nameWithOwner } } \
        ... on Label { repository { nameWithOwner } } \
        ... on Milestone { repository { nameWithOwner } } } }
        """

    private static let cache = Mutex<[String: String?]>([:])

    /// The GraphQL endpoint on `host`, the host the request came to, so the token goes nowhere it is not bound to.
    static func endpoint(_ host: String) -> URL? {
        let host = host.lowercased()
        return URL(string: host == "api.github.com" ? "https://api.github.com/graphql" : "https://\(host)/api/graphql")
    }

    /// Each ID's repository (`owner/name`, lower case), nil for a node of an `unowned` type; throws when one is neither.
    @Sendable static func resolve(host: String, token: String, ids: [String]) async throws -> [String: String?] {
        var answer: [String: String?] = [:]
        let key = { (id: String) in host.lowercased() + " " + id }
        var missing: [String] = []
        cache.withLock { c in
            for id in ids { if let known = c[key(id)] { answer[id] = known } else { missing.append(id) } }
        }
        guard !missing.isEmpty else { return answer }
        guard let url = endpoint(host) else { throw Failure("no GraphQL endpoint for \(host)") }
        var request = URLRequest(url: url, timeoutInterval: 10)
        request.httpMethod = "POST"
        request.setValue("bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONSerialization.data(withJSONObject: ["query": query, "variables": ["ids": missing]])
        let (data, response) = try await URLSession.shared.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 200 else {
            throw Failure("looking up node IDs answered HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)")
        }
        let found = try parse(data, ids: missing)
        cache.withLock { c in for (id, repo) in found { c[key(id)] = repo } }
        return answer.merging(found) { $1 }
    }

    /// The `nodes` answer to `query`, in the order of `ids`.
    static func parse(_ data: Data, ids: [String]) throws -> [String: String?] {
        guard let json = try JSONSerialization.jsonObject(with: data) as? [String: Any], let nodes = (json["data"] as? [String: Any])?["nodes"] as? [Any],
              nodes.count == ids.count
        else { throw Failure("looking up node IDs gave no answer") }
        var out: [String: String?] = [:]
        for (id, node) in zip(ids, nodes) {
            guard let node = node as? [String: Any], let type = node["__typename"] as? String else { throw Failure("node \(id) does not exist") }
            if let name = (node["nameWithOwner"] as? String) ?? ((node["repository"] as? [String: Any])?["nameWithOwner"] as? String) {
                out[id] = .some(name.lowercased())
            } else if unowned.contains(type) {
                out[id] = .some(nil)
            } else {
                throw Failure("node \(id) is a \(type), which the proxy cannot tie to a repository")
            }
        }
        return out
    }

    struct Failure: Error, CustomStringConvertible {
        var description: String
        init(_ description: String) { self.description = description }
    }
}
