import Foundation

/// A project's VM in git mode (`VMConfig.Project.Sync.git`): the guest has its own repository at the project's path, on
/// its own disk. `glov <harness>` pushes the Mac's current branch there and fetches the guest's branches back as
/// `refs/remotes/glov-<vm>/*`; the guest never pushes to the Mac. Git reaches the guest over SSH as the host `glov-<vm>`,
/// which glov writes in its own ssh_config (`withSSHHost`).
public enum ProjectGit {
    /// The Mac repository's remote for VM `vm`.
    public static func remote(vm: String) -> String { "glov-" + vm }
    /// The SSH host of VM `vm` in glov's ssh_config.
    public static func host(vm: String) -> String { "glov-" + vm }

    /// Shown whenever live mode is picked, and as a reminder on each start of a live VM.
    public static let liveWarning = "warning: the guest can change anything in the project and its git data; hooks, git "
        + "config, .envrc and build scripts it writes run on this Mac when you use them there. Review them first."

    /// What the first run's `[git/live]` prompt makes of `answer`: Enter is git; nil for anything else.
    public static func choice(_ answer: String) -> VMConfig.Project.Sync? {
        switch answer.trimmingCharacters(in: .whitespaces).lowercased() {
        case "", "git": .git
        case "live": .live
        default: nil
        }
    }

    /// Makes `$1` the guest's repository of the project, as the guest user: a new repository takes pushes to its
    /// checked-out branch (`$2`) into the working tree. It commits as the owner (`$3`, `$4`), unsigned: the owner's key
    /// stays on the Mac, and `glov vm land` signs. `$5` is the Mac's origin, if any. No hooks are installed.
    public static let guestRepoScript = """
        set -e
        command -v git >/dev/null || { echo "git is not installed in the guest" >&2; exit 3; }
        cd "$1"
        if [ ! -e .git ]; then
        \tgit init -q
        \tgit config receive.denyCurrentBranch updateInstead
        \tgit symbolic-ref HEAD "refs/heads/$2"
        fi
        git config user.name "$3"
        git config user.email "$4"
        git config commit.gpgsign false
        git config tag.gpgsign false
        if [ -n "$5" ]; then
        \tgit remote set-url origin "$5" 2>/dev/null || git remote add origin "$5"
        fi

        """

    /// Checks the guest's repository (`$1`) out on branch `$2`, unless it is there already.
    public static let guestBranchScript = """
        cd "$1" || exit 1
        [ "$(git symbolic-ref --short -q HEAD)" = "$2" ] && exit 0
        git checkout -q "$2"

        """

    /// Exits 0 and prints the name of a program of the guest user's that works in `$1` or below (its current
    /// directory there), such as a harness; exits 1 when there is none. A shell idle at its prompt there (no child, as
    /// one left in a herdr tab or a `glov ssh` session) does not count: what it runs next is in the project too and
    /// counts then. Linux (`/proc`).
    public static let guestBusyScript = """
        for p in /proc/[0-9]*; do
        	pid=${p#/proc/}
        	[ "$pid" = "$$" ] && continue
        	c=$(readlink "$p/cwd" 2>/dev/null) || continue
        	case "$c" in "$1"|"$1"/*) ;; *) continue ;; esac
        	n=$(cat "$p/comm" 2>/dev/null) || continue
        	case "$n" in sh|bash|dash|zsh|fish|ksh|mksh)
        		grep -qs "^PPid:[[:space:]]*$pid$" /proc/[0-9]*/status || continue ;;
        	esac
        	echo "$n"
        	exit 0
        done
        exit 1

        """

    /// One VM's block in glov's ssh_config.
    public struct SSHHost: Equatable, Sendable {
        public var vm: String
        /// The VM's private key and known_hosts, in its directory.
        public var key: String
        public var knownHosts: String
        /// The ProxyCommand (`glov ssh-relay <vm>`), already quoted for a shell.
        public var relay: String

        public init(vm: String, key: String, knownHosts: String, relay: String) {
            self.vm = vm
            self.key = key
            self.knownHosts = knownHosts
            self.relay = relay
        }
    }

    static func begin(_ vm: String) -> String { "# glov \(vm): begin (written by glov)" }
    static func end(_ vm: String) -> String { "# glov \(vm): end" }

    /// glov's ssh_config `text` with `h`'s block in place of the VM's old one, or added. The forwarding, escape and
    /// master-connection options keep the guest from reaching the Mac, as `glov ssh` does for an isolated VM. ssh keeps
    /// an option's first value, and the Include goes first in ~/.ssh/config, so a `Host *` there cannot loosen them.
    public static func withSSHHost(_ text: String, _ h: SSHHost) -> String {
        var out = withoutSSHHost(text, vm: h.vm)
        let block = [
            begin(h.vm),
            "Host " + host(vm: h.vm),
            "  HostName " + h.vm,
            "  User " + CloudInitSeed.user,
            "  IdentityFile " + quoted(h.key),
            "  IdentitiesOnly yes",
            "  UserKnownHostsFile " + quoted(h.knownHosts),
            "  StrictHostKeyChecking accept-new",
            "  HostKeyAlias " + h.vm,
            "  ProxyCommand " + h.relay,
            "  ClearAllForwardings yes",
            "  ForwardAgent no",
            "  ForwardX11 no",
            "  Tunnel no",
            "  PermitLocalCommand no",
            "  EnableEscapeCommandline no",
            "  ControlMaster no",
            "  ControlPath none",
            end(h.vm),
        ]
        if !out.isEmpty && !out.hasSuffix("\n") { out += "\n" }
        return out + block.joined(separator: "\n") + "\n"
    }

    /// glov's ssh_config `text` without VM `vm`'s block.
    public static func withoutSSHHost(_ text: String, vm: String) -> String {
        var out: [Substring] = []
        var skipping = false
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            if line == begin(vm) { skipping = true; continue }
            if line == end(vm), skipping { skipping = false; continue }
            if !skipping { out.append(line) }
        }
        return out.joined(separator: "\n")
    }

    /// Whether glov's ssh_config `text` has VM `vm`'s host.
    public static func hasSSHHost(_ text: String, vm: String) -> Bool {
        text.split(separator: "\n").contains { $0 == "Host " + host(vm: vm) }
    }

    /// A path in an ssh_config value: ssh splits at spaces (the default glov home is `~/Library/Application Support/glov`)
    /// and expands `%` tokens in file options.
    static func quoted(_ path: String) -> String {
        "\"" + path.replacingOccurrences(of: "%", with: "%%").replacingOccurrences(of: "\"", with: "\\\"") + "\""
    }

    /// How ~/.ssh/config (in `home`) names glov's ssh_config at `path`: from `~` when `path` is in `home`.
    static func includePath(_ path: String, home: String) -> String {
        let h = home.hasSuffix("/") ? String(home.dropLast()) : home
        return !h.isEmpty && path.hasPrefix(h + "/") ? "~" + path.dropFirst(h.count) : path
    }

    /// The line ~/.ssh/config (in `home`) needs for ssh to read glov's ssh_config at `path`.
    public static func includeLine(path: String, home: String) -> String { "Include \"\(includePath(path, home: home))\"" }

    /// Whether ~/.ssh/config `text` (in `home`) includes `path`, written from `~` or in full, quoted or not.
    public static func hasInclude(_ text: String, path: String, home: String) -> Bool {
        let wanted: Set<String> = [path, includePath(path, home: home)]
        for line in text.split(separator: "\n") {
            let words = configWords(line.trimmingCharacters(in: .whitespaces))
            if words.count >= 2, words[0].lowercased() == "include", words.dropFirst().contains(where: wanted.contains) { return true }
        }
        return false
    }

    /// ~/.ssh/config `text` (in `home`) with the Include first: one after a `Host` line would apply only within that host.
    public static func withInclude(_ text: String, path: String, home: String) -> String {
        "# Added by glov: the SSH hosts of glov claude's VMs (glov-<vm>)\n" + includeLine(path: path, home: home) + "\n\n" + text
    }

    /// An ssh_config line's words, with double-quoted ones taken whole and their quotes removed.
    static func configWords(_ line: String) -> [String] {
        var words: [String] = [], word = "", quoting = false, any = false
        for ch in line {
            if ch == "\"" { quoting.toggle(); any = true; continue }
            if !quoting, ch == " " || ch == "\t" || ch == "=" {
                if any { words.append(word); word = ""; any = false }
                continue
            }
            word.append(ch)
            any = true
        }
        if any { words.append(word) }
        return words
    }

    /// A remote URL without a user or password written into an http(s) URL (`https://user:token@host/...`, or a token
    /// alone as the user), which must not reach the guest. An SSH URL's user (`ssh://git@` or scp-like `git@host:path`)
    /// is the account, not a secret, and is kept.
    public static func withoutCredentials(_ url: String) -> String {
        guard var parts = URLComponents(string: url), ["http", "https"].contains(parts.scheme?.lowercased() ?? ""),
            parts.user != nil || parts.password != nil
        else { return url }
        parts.user = nil
        parts.password = nil
        return parts.string ?? ""
    }

    /// The commits `git cherry <upstream> <head>` lists as missing from upstream (its `+` lines), oldest first.
    public static func cherryPicks(_ output: String) -> [String] {
        output.split(separator: "\n").compactMap { $0.hasPrefix("+ ") ? String($0.dropFirst(2)) : nil }
    }
}
