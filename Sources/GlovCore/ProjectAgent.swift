import CryptoKit
import Foundation

/// `glov <harness>` in a project: an isolated VM of the project's own, with the working tree shared at the same path in
/// the guest, and the harness in a tmux session there.
public enum ProjectAgent {
    /// The share that holds the project; its host path tells which project a VM belongs to.
    public static let shareName = "project"

    /// The git root of `directory` (the nearest ancestor holding `.git`, a directory or a worktree's file), else the
    /// directory itself, with links resolved. Found without running git, which reads the repository's config.
    public static func root(of directory: String) -> String {
        let start = HostPath.realpath(directory) ?? directory
        var dir = start
        while true {
            if FileManager.default.fileExists(atPath: dir + "/.git") { return dir }
            let parent = (dir as NSString).deletingLastPathComponent
            if parent == dir || parent.isEmpty { return start }
            dir = parent
        }
    }

    /// The share that holds a worktree's repository (`gitDirectory`).
    public static let gitShareName = "project-git"

    /// For a git worktree, whose `.git` is a file naming its directory in another repository, that repository's common
    /// directory (the main checkout's `.git`): git in the guest needs it at the same path. Nil for a plain repository or
    /// none, and for anything that is not plainly a worktree of a repository: the `.git` file is the project's (or a
    /// former guest's) to write, so it must not choose a host folder to share. The worktree's directory must be
    /// `<common>/worktrees/<name>` and name this project's `.git` back, and the common directory must hold a repository.
    /// The repositories the project's git remotes name (`GitSignIn.repo(remote:)`), the default of its VM's
    /// `glov secret repos`. Read once, at the VM's creation: a remote the guest adds later is not let in by itself.
    public static func remoteRepos(project: String) -> [RepoPattern] {
        let dir = gitDirectory(project: project) ?? project + "/.git"
        guard let text = try? String(contentsOfFile: dir + "/config", encoding: .utf8) else { return [] }
        var out: [RepoPattern] = []
        for p in GitSignIn.remoteURLs(gitConfig: text).compactMap(GitSignIn.repo(remote:)) where !out.contains(p) { out.append(p) }
        return out
    }

    public static func gitDirectory(project: String) -> String? {
        let fm = FileManager.default
        let dotGit = project + "/.git"
        var isDir = ObjCBool(false)
        guard fm.fileExists(atPath: dotGit, isDirectory: &isDir), !isDir.boolValue,
            (try? fm.destinationOfSymbolicLink(atPath: dotGit)) == nil,
            let text = try? String(contentsOfFile: dotGit, encoding: .utf8),
            let line = text.split(whereSeparator: \.isNewline).first, line.hasPrefix("gitdir:")
        else { return nil }
        var named = line.dropFirst("gitdir:".count).trimmingCharacters(in: .whitespaces)
        if !named.hasPrefix("/") { named = project + "/" + named }
        guard let worktree = HostPath.realpath(named) else { return nil }
        let parent = (worktree as NSString).deletingLastPathComponent
        guard (parent as NSString).lastPathComponent == "worktrees" else { return nil }
        let common = (parent as NSString).deletingLastPathComponent
        // The worktree's directory names the project's .git back.
        guard let back = try? String(contentsOfFile: worktree + "/gitdir", encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines),
            HostPath.realpath(back) == HostPath.realpath(dotGit)
        else { return nil }
        for (entry, dir) in [("HEAD", false), ("objects", true), ("refs", true)] {
            var isDirectory = ObjCBool(false)
            guard fm.fileExists(atPath: common + "/" + entry, isDirectory: &isDirectory), isDirectory.boolValue == dir else { return nil }
        }
        return common
    }

    /// The project's VM: its folder's name made a valid VM name, and a hash of the whole path so two folders of one name
    /// get two VMs.
    public static func vmName(project: String) -> String {
        var base = String((project as NSString).lastPathComponent.lowercased().unicodeScalars.map { c -> Character in
            (c >= "a" && c <= "z") || (c >= "0" && c <= "9") ? Character(c) : "-"
        })
        while base.contains("--") { base = base.replacingOccurrences(of: "--", with: "-") }
        base = String(base.trimmingCharacters(in: CharacterSet(charactersIn: "-")).prefix(24))
        while base.hasSuffix("-") { base.removeLast() }
        let hash = SHA256.hash(data: Data(project.utf8)).prefix(3).map { String(format: "%02x", $0) }.joined()
        return (base.isEmpty ? "project" : base) + "-" + hash
    }

    /// The tmux session to attach: the harness's own, or with `new` the first of `<harness>-2`, `<harness>-3`... not in
    /// `existing`.
    public static func session(harness: String, existing: Set<String>, new: Bool) -> String {
        guard new else { return harness }
        var n = 2
        while existing.contains("\(harness)-\(n)") { n += 1 }
        return "\(harness)-\(n)"
    }

    /// The remote command of the ssh session: attach `session`, or start it running `argv` in `directory`. Each harness
    /// has a tmux server (`-L`) of its own, which takes the environment of the sign-in that starts it: a harness added to
    /// the VM later gets its variables even while another harness's sessions run.
    public static func tmuxCommand(server: String, session: String, directory: String, argv: [String]) -> String {
        (["exec", "tmux", "-L", server, "new-session", "-A", "-s", session, "-c", directory, "--"] + argv).map(shellQuote).joined(separator: " ")
    }

    public static func shellQuote(_ s: String) -> String {
        if !s.isEmpty, s.unicodeScalars.allSatisfy({ CharacterSet.alphanumerics.contains($0) || "-_./=:@%+,".unicodeScalars.contains($0) }) {
            return s
        }
        return "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    /// The harness's arguments from what glov did not take: ArgumentParser keeps the `--` that ends glov's options, which
    /// is glov's, not the harness's (a second one is passed on).
    public static func harnessArguments(_ unrecognized: [String]) -> [String] {
        guard let i = unrecognized.firstIndex(of: "--") else { return unrecognized }
        var a = unrecognized
        a.remove(at: i)
        return a
    }

    /// The harness `glov <word>` names: a profile's command (`agy`) or name (`antigravity`).
    public static func harness(for word: String) -> HarnessProfile? {
        HarnessProfile.all.first { $0.command == word } ?? HarnessProfile.all.first { $0.name == word }
    }
}
