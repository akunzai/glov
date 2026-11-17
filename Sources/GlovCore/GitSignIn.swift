import Foundation

/// What an isolated VM's guest holds so git, gh and glab sign in with the VM's git secrets (`glov secret source --forge`)
/// without a token entering it: each tool gets the placeholder `glov-secret:<name>` where the token goes, and the egress
/// proxy puts the value in on the way to the secret's hosts (docs/networking.md "Repositories"). Pure renderers; the
/// host writes them through the Guest Service.
public enum GitSignIn {
    /// One git secret granted to the VM.
    public struct Entry: Sendable, Equatable {
        public var name: String
        public var forge: Forge
        /// The secret's hosts (`glov secret source --host`).
        public var hosts: [String]

        public init(name: String, forge: Forge, hosts: [String]) {
            self.name = name
            self.forge = forge
            self.hosts = hosts
        }

        /// The hosts git and the CLIs name repositories under: GitHub's API hosts fold into github.com; wildcards, IP
        /// literals and ports other than 443 name no git host.
        public var gitHosts: [String] {
            var out: [String] = []
            for h in hosts {
                var host = h.lowercased()
                if let colon = host.lastIndex(of: ":") {
                    guard host[host.index(after: colon)...] == "443" else { continue }
                    host = String(host[..<colon])
                }
                guard !host.contains("*"), !host.allSatisfy({ $0.isNumber || $0 == "." }), host.contains(".") else { continue }
                let git = RepoAccess.repoHost(forge, host)
                if !out.contains(git) { out.append(git) }
            }
            return out
        }
    }

    /// git asks `git-credential-glov` (`credential.helper = glov`) for every HTTPS host; it answers for the hosts in
    /// `signInsPath` only.
    public static let helperPath = "/usr/local/bin/git-credential-glov"
    public static let signInsPath = "/etc/glov/git-sign-ins"
    public static let gitConfigPath = "/etc/gitconfig"
    /// Relative to the guest user's home.
    public static let ghHostsPath = ".config/gh/hosts.yml"
    public static let glabConfigPath = ".config/glab-cli/config.yml"

    public static let helper = """
        #!/bin/sh
        # Written by glov. Answers git with the placeholder of the VM's git secret for the host; the egress proxy on the
        # Mac puts the token in, so it never enters the VM.
        [ "$1" = get ] || exit 0
        host= protocol=
        while IFS= read -r line && [ -n "$line" ]; do
          case $line in host=*) host=${line#host=} ;; protocol=*) protocol=${line#protocol=} ;; esac
        done
        [ "$protocol" = https ] || exit 0
        [ -r \(signInsPath) ] || exit 0
        while read -r h forge name; do
          [ "$h" = "$host" ] || continue
          case $forge in gitlab) user=oauth2 ;; *) user=x-access-token ;; esac
          printf 'username=%s\\npassword=glov-secret:%s\\n' "$user" "$name"
          exit 0
        done < \(signInsPath)

        """

    /// `signInsPath`: one `<host> <forge> <secret>` line per git host, the first secret for a host winning.
    public static func signIns(_ entries: [Entry]) -> String {
        var seen = Set<String>(), lines: [String] = []
        for e in entries.sorted(by: { $0.name < $1.name }) {
            for h in e.gitHosts where seen.insert(h).inserted { lines.append("\(h) \(e.forge.rawValue) \(e.name)\n") }
        }
        return lines.joined()
    }

    static let begin = "# glov git sign-in: begin (written by glov; edits between these lines are replaced)"
    static let end = "# glov git sign-in: end"

    /// `/etc/gitconfig` (`text`) with glov's block: the credential helper, and SSH remotes of the git hosts rewritten
    /// to HTTPS (no SSH key reaches the VM, and the proxy can only read HTTPS). Without entries the block is removed;
    /// other lines stay as they are.
    public static func gitConfig(_ text: String, entries: [Entry]) -> String {
        var lines = text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        if lines.last == "" { lines.removeLast() }
        if let b = lines.firstIndex(of: begin), let e = lines[b...].firstIndex(of: end) { lines.removeSubrange(b...e) }
        let hosts = entries.sorted { $0.name < $1.name }.flatMap(\.gitHosts).reduce(into: [String]()) { if !$0.contains($1) { $0.append($1) } }
        if !hosts.isEmpty {
            lines.append(begin)
            lines += ["[credential]", "\thelper = glov"]
            for h in hosts { lines += ["[url \"https://\(h)/\"]", "\tinsteadOf = git@\(h):", "\tinsteadOf = ssh://git@\(h)/"] }
            lines.append(end)
        }
        return lines.map { $0 + "\n" }.joined()
    }

    /// gh's `hosts.yml` for the GitHub entries, nil without any.
    public static func ghHosts(_ entries: [Entry]) -> String? {
        let hosts = pairs(entries, .github)
        guard !hosts.isEmpty else { return nil }
        return "# Written by glov: the token is a placeholder the egress proxy on the Mac fills in.\n" + hosts.map { host, name in
            """
            \(host):
                git_protocol: https
                oauth_token: glov-secret:\(name)
                user: glov
                users:
                    glov:
                        oauth_token: glov-secret:\(name)

            """
        }.joined()
    }

    /// glab's `config.yml` for the GitLab entries, nil without any; the first host is glab's default. `is_oauth2` makes
    /// glab send `Authorization: Bearer`, which GitLab takes for an OAuth token (what `glab auth login` stores) and a
    /// personal access token alike; without it an OAuth token goes as `PRIVATE-TOKEN` and gets `401`. glab also parses
    /// `oauth2_expiry_date` for an OAuth entry and fails on an empty one, so the entry carries a fixed far-future date in
    /// RFC 822 form (glab 1.53 reads only that; later ones also RFC 3339): the real token is swapped and refreshed on
    /// the Mac. The year is 68 because Go reads
    /// two-digit years 69-99 as 19xx and 00-68 as 20xx.
    public static func glabConfig(_ entries: [Entry]) -> String? {
        let hosts = pairs(entries, .gitlab)
        guard let first = hosts.first else { return nil }
        return "# Written by glov: the token is a placeholder the egress proxy on the Mac fills in.\n"
            + "git_protocol: https\ncheck_update: false\ntelemetry: false\nhost: \(first.host)\nhosts:\n"
            + hosts.map { host, name in
                """
                    \(host):
                        api_protocol: https
                        api_host: \(host)
                        git_protocol: https
                        is_oauth2: "true"
                        oauth2_expiry_date: 01 Jan 68 00:00 UTC
                        token: glov-secret:\(name)

                """
            }.joined()
    }

    private static func pairs(_ entries: [Entry], _ forge: Forge) -> [(host: String, name: String)] {
        var seen = Set<String>(), out: [(String, String)] = []
        for e in entries.sorted(by: { $0.name < $1.name }) where e.forge == forge {
            for h in e.gitHosts where seen.insert(h).inserted { out.append((h, e.name)) }
        }
        return out
    }

    /// The `url` of each remote in a git `config` file, in order.
    public static func remoteURLs(gitConfig text: String) -> [String] {
        var inRemote = false, out: [String] = []
        for raw in text.split(whereSeparator: \.isNewline) {
            let line = raw.trimmingCharacters(in: .whitespaces)
            if line.hasPrefix("[") { inRemote = line.hasPrefix("[remote "); continue }
            guard inRemote, let eq = line.firstIndex(of: "="), line[..<eq].trimmingCharacters(in: .whitespaces).lowercased() == "url" else { continue }
            var v = line[line.index(after: eq)...].trimmingCharacters(in: .whitespaces)
            if v.count >= 2, v.hasPrefix("\""), v.hasSuffix("\"") { v = String(v.dropFirst().dropLast()) }
            out.append(v)
        }
        return out
    }

    /// The repository a remote URL names (`https://host/o/r.git`, `ssh://git@host/o/r`, `git@host:o/r.git`), nil for
    /// a local path, a wildcard or anything else. The project's `.git/config` is writable by any guest it was shared
    /// with, so a remote never names more than one repository.
    public static func repo(remote url: String) -> RepoPattern? {
        var rest: Substring
        if let scheme = url.range(of: "://") {
            guard ["https", "http", "ssh", "git+ssh"].contains(url[..<scheme.lowerBound].lowercased()) else { return nil }
            rest = url[scheme.upperBound...]
            guard let slash = rest.firstIndex(of: "/") else { return nil }
            var authority = rest[..<slash]
            if let at = authority.lastIndex(of: "@") { authority = authority[authority.index(after: at)...] }
            if let colon = authority.lastIndex(of: ":") { authority = authority[..<colon] }
            rest = authority + rest[slash...]
        } else if let colon = url.firstIndex(of: ":"), !url[..<colon].contains("/") {
            // scp-like: [user@]host:path
            var host = url[..<colon]
            if let at = host.lastIndex(of: "@") { host = host[host.index(after: at)...] }
            rest = host + "/" + url[url.index(after: colon)...].drop { $0 == "/" }
        } else {
            return nil
        }
        // A remote names one repository: a `*` would let a remote URL allow a whole owner or group.
        guard !rest.contains("*") else { return nil }
        return RepoPattern(String(rest))
    }
}
