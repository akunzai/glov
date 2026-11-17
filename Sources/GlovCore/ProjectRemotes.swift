import Foundation

/// The project remotes the owner has let a project's VM reach (`glov claude` on a new VM): kept on the Mac, since the
/// project's `.git/config` is writable by every guest it was shared with, so a remote a guest added shows as new.
/// `<home>/project-remotes.json` maps a project's path to its confirmed repositories (`RepoPattern.text`).
public struct ProjectRemotes: Sendable {
    public let path: String

    /// `home`: the glov home directory.
    public init(home: String) { path = home + "/project-remotes.json" }

    public func confirmed(_ project: String) -> Set<String> { Set(load()[project] ?? []) }

    /// Adds `repos` to the project's confirmed ones.
    public func confirm(_ project: String, _ repos: [RepoPattern]) throws {
        var all = load()
        all[project] = Array(Set(all[project] ?? []).union(repos.map(\.text))).sorted()
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        try encoder.encode(all).write(to: URL(fileURLWithPath: path), options: .atomic)
    }

    private func load() -> [String: [String]] {
        guard let data = FileManager.default.contents(atPath: path) else { return [:] }
        return (try? JSONDecoder().decode([String: [String]].self, from: data)) ?? [:]
    }

    /// Whether to apply the remotes: at a terminal the owner's `answer` (Enter accepts unless one is new, that is not
    /// confirmed for the project before); without one, only with `--allow-remotes`.
    public static func apply(anyNew: Bool, isTerminal: Bool, allowFlag: Bool, answer: () -> String?) -> Bool {
        if allowFlag { return true }
        guard isTerminal else { return false }
        let a = (answer() ?? "").trimmingCharacters(in: .whitespaces).lowercased()
        if a.isEmpty { return !anyNew }
        return ["y", "yes"].contains(a)
    }
}
