import Foundation
import GlovCore

/// The Mac's repository of a git-mode project (`ProjectGit`): git run in it, reaching VM `vm`'s guest repository over SSH
/// through glov's ssh_config. Nothing of the guest's runs here: fetch runs no hooks of the guest's and copies no config.
public struct ProjectRepo: Sendable {
    public let dir: String
    public let vm: String
    public let home: GlovHome
    /// Where warnings and progress go (stderr).
    let log: @Sendable (String) -> Void

    public init(dir: String, vm: String, home: GlovHome, log: @escaping @Sendable (String) -> Void = { FileHandle.standardError.write(Data(($0 + "\n").utf8)) }) {
        self.dir = dir
        self.vm = vm
        self.home = home
        self.log = log
    }

    public struct Failure: Error, CustomStringConvertible {
        public let description: String
        init(_ description: String) { self.description = description }
    }

    /// Runs a command in the guest as `as` ("system" or "user"); its exit code and output tail.
    public typealias GuestRun = (_ argv: [String], _ as: String) async throws -> (code: Int, tail: String)

    public var remote: String { ProjectGit.remote(vm: vm) }
    public var sshConfigPath: String { home.path + "/ssh_config" }

    /// git in the project, its output trimmed; throws with git's stderr when it fails. It waits through a termination
    /// handler and reads the pipes off the cooperative pool (`Blocking`).
    @discardableResult
    public func git(_ arguments: [String]) async throws -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: try Self.gitPath())
        p.arguments = ["-C", dir] + arguments
        var env = ProcessInfo.processInfo.environment
        // glov's ssh_config, so git reaches glov-<vm> whether or not ~/.ssh/config includes it.
        env["GIT_SSH_COMMAND"] = "ssh -F " + ProjectAgent.shellQuote(sshConfigPath)
        env["GIT_TERMINAL_PROMPT"] = "0"
        p.environment = env
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        p.standardInput = FileHandle.nullDevice
        let (exited, exit) = AsyncStream<Void>.makeStream()
        p.terminationHandler = { _ in exit.yield(); exit.finish() }
        try p.run()
        // Both pipes at once: one left unread can fill and stall git.
        async let output = Blocking.run { out.fileHandleForReading.readDataToEndOfFile() }
        async let errorOutput = Blocking.run { err.fileHandleForReading.readDataToEndOfFile() }
        let (data, errors) = await (output, errorOutput)
        for await _ in exited {}
        guard p.terminationStatus == 0 else {
            let detail = String(decoding: errors, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            throw Failure("git \(arguments.prefix(2).joined(separator: " ")) failed" + (detail.isEmpty ? "" : ": \(detail)"))
        }
        return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Whether git succeeds.
    public func gitSucceeds(_ arguments: [String]) async -> Bool { (try? await git(arguments)) != nil }

    /// The checked-out branch; nil for a detached HEAD.
    public func currentBranch() async -> String? {
        (try? await git(["symbolic-ref", "--short", "-q", "HEAD"])).flatMap { $0.isEmpty ? nil : $0 }
    }

    /// Fetches from the guest. The guest's branches land only under refs/remotes/glov-<vm>/*; git runs no hook of the
    /// guest's, and glov keeps it from following the guest's `.gitmodules` into submodule fetches.
    private func fetchGuest() async throws {
        try await git(["-c", "fetch.recurseSubmodules=false", "fetch", "-q", remote])
    }

    /// Sets up git's way into the VM: its host in glov's ssh_config (and an Include in ~/.ssh/config once the owner
    /// agrees), the guest's repository at `guest`, and the remote `glov-<vm>` here.
    public func connect(config: VMConfig, guest: String, run: GuestRun) async throws {
        try writeSSHHost(config: config)
        offerInclude()
        let made = try await run(["install", "-d", "-o", CloudInitSeed.user, "-g", CloudInitSeed.user, guest], "system")
        guard made.code == 0 else { throw Failure("making \(guest) in the guest exited \(made.code): \(made.tail)") }
        let name = (try? await git(["config", "user.name"])) ?? ""
        let email = (try? await git(["config", "user.email"])) ?? ""
        // A sign-in written into the URL stays here: the guest gets the forge's placeholder through glov secret instead.
        let origin = ProjectGit.withoutCredentials((try? await git(["config", "--get", "remote.origin.url"])) ?? "")
        let set = try await run(["/bin/sh", "-c", ProjectGit.guestRepoScript, "sh", guest, await currentBranch() ?? "main", name, email, origin], "user")
        guard set.code == 0 else { throw Failure("setting up the guest's repository exited \(set.code): \(set.tail)") }
        let url = ProjectGit.host(vm: vm) + ":" + guest
        if let current = try? await git(["remote", "get-url", remote]) {
            if current != url { try await git(["remote", "set-url", remote, url]) }
        } else {
            try await git(["remote", "add", remote, url])
            say("added the remote \(remote) (\(url)) to \(dir)")
        }
        // The guest's tags stay out of the project's own.
        try await git(["config", "remote.\(remote).tagOpt", "--no-tags"])
    }

    /// Sends the current branch to the guest and checks it out there; the branch, nil for a detached HEAD. It fetches
    /// first and pushes only what the guest's branch lacks: a guest ahead keeps its commits, and one whose branch went
    /// another way, or with changes not committed, keeps its state and glov says so. It never forces.
    public func push(guest: String, run: GuestRun) async -> String? {
        guard let branch = await currentBranch() else {
            say("warning: HEAD is detached on this Mac; nothing pushed to the guest")
            return nil
        }
        // A push moves the guest's working tree (updateInstead), and a checkout switches it: not under a harness at work.
        if let busy = try? await run(["/bin/sh", "-c", ProjectGit.guestBusyScript, "sh", guest], "user"), busy.code == 0 {
            let program = busy.tail.trimmingCharacters(in: .whitespacesAndNewlines)
            say("not pushed: \(program.isEmpty ? "a program" : program) runs in \(guest) in the guest, whose tree stays as it is; "
                + "end it (exit the harness, or detach and close what runs there), then run again to push \(branch)")
            return branch
        }
        if let status = try? await git(["status", "--porcelain"]), !status.isEmpty {
            let n = status.split(separator: "\n").count
            say("warning: \(n) change\(n == 1 ? "" : "s") not committed stay\(n == 1 ? "s" : "") on this Mac; the guest gets only commits")
        }
        do { try await fetchGuest() } catch { say("warning: fetching from the guest failed: \(error)") }
        let ref = "refs/heads/" + branch, theirs = "refs/remotes/\(remote)/\(branch)"
        if await gitSucceeds(["rev-parse", "--verify", "-q", theirs]) {
            if await gitSucceeds(["merge-base", "--is-ancestor", ref, theirs]) {
                // The guest has all of it, and maybe more: nothing to send.
                return await checkout(guest: guest, branch: branch, run: run)
            }
            if await !gitSucceeds(["merge-base", "--is-ancestor", theirs, ref]) {
                say("warning: \(branch) and \(remote)/\(branch) have diverged; the guest keeps its own (git log \(branch)...\(remote)/\(branch))")
                return await checkout(guest: guest, branch: branch, run: run)
            }
        }
        // --no-verify: this Mac's pre-push hooks are for its forges (git-lfs's would try to upload to the guest).
        do { try await git(["push", "-q", "--no-verify", remote, ref + ":" + ref]) } catch {
            say("warning: \(branch) was not pushed to the guest, which keeps its own state: \(error)")
            return branch
        }
        say("pushed \(branch) to the guest")
        return await checkout(guest: guest, branch: branch, run: run)
    }

    /// Puts the guest's repository on `branch`, unless changes not committed there keep it where it is.
    private func checkout(guest: String, branch: String, run: GuestRun) async -> String {
        let r = try? await run(["/bin/sh", "-c", ProjectGit.guestBranchScript, "sh", guest, branch], "user")
        if r?.code != 0 { say("warning: the guest stays on its own branch, not \(branch): \(r?.tail ?? "")") }
        return branch
    }

    /// Brings the guest's branches back as `refs/remotes/glov-<vm>/*` and says how far the guest's `branch` is ahead.
    @discardableResult
    public func fetch(branch: String?) async -> Bool {
        do { try await fetchGuest() } catch {
            say("warning: fetching from the guest failed: \(error)")
            return false
        }
        if let branch, let n = try? await git(["rev-list", "--count", "\(branch)..\(remote)/\(branch)"]), n != "0" {
            say("\(remote)/\(branch) is \(n) commit\(n == "1" ? "" : "s") ahead of \(branch); review with git log \(branch)..\(remote)/\(branch)")
        }
        return true
    }

    /// While `glov vm land` is under way, the guest's commit it lands: fetches leave it alone, and the push back expects
    /// the guest's branch there still.
    func landRef(_ branch: String) -> String { "refs/glov-land/\(vm)/\(branch)" }
    /// The local branch as it was before the land: once the owner finished a stopped cherry-pick the branch is past it,
    /// and if they aborted it, it is there.
    func landBase(_ branch: String) -> String { landRef(branch) + ".base" }
    /// The guest's commit last landed whose push back the guest refused, having committed since: the next land takes
    /// only what follows it, since a commit whose conflict the owner resolved differs from the guest's.
    func landedRef(_ branch: String) -> String { landRef(branch) + ".landed" }

    /// `glov vm land`: the guest's new commits on `requested` (the checked-out branch by default) made again on the local
    /// branch once the owner agrees (`confirm` gets the question), so they are the owner's commits, signed as the owner's
    /// git config signs; then the branch goes back to the guest, which may push it and open a pull request with the
    /// signatures intact.
    public func land(branch requested: String?, confirm: (_ question: String) async throws -> Bool) async throws {
        let current = await currentBranch()
        guard let branch = requested ?? current, branch == current else {
            throw Failure("check \(requested ?? "a branch") out in \(dir) first: glov vm land makes the guest's commits again on the checked-out branch")
        }
        if await pickInProgress() {
            throw Failure("a cherry-pick is under way in \(dir): resolve it and git cherry-pick --continue (or --abort), then glov vm land \(vm) again")
        }
        let theirs = "refs/remotes/\(remote)/\(branch)", pending = landRef(branch)

        // A land that stopped at a conflict, now finished: only the push back is left.
        if let sha = try? await git(["rev-parse", "--verify", "-q", pending]) {
            let base = (try? await git(["rev-parse", "--verify", "-q", landBase(branch)])) ?? ""
            let tip = (try? await git(["rev-parse", "HEAD"])) ?? ""
            if !base.isEmpty, tip != base, await gitSucceeds(["merge-base", "--is-ancestor", base, tip]) {
                return try await pushBack(branch, sha)
            }
            await dropLandRefs(branch)
        }

        guard await fetch(branch: nil) else { throw Failure("fetching from \(vm) failed") }
        guard let sha = try? await git(["rev-parse", "--verify", "-q", theirs]) else { throw Failure("the guest has no branch \(branch)") }
        var limit = (try? await git(["rev-parse", "--verify", "-q", landedRef(branch)])) ?? ""
        if !limit.isEmpty, await !gitSucceeds(["merge-base", "--is-ancestor", limit, sha]) { limit = "" }  // the guest's branch went another way since
        let merges = (try? await git(["rev-list", "--merges", sha, "^" + branch, "^" + (limit.isEmpty ? branch : limit)])) ?? ""
        guard merges.isEmpty else {
            throw Failure("\(remote)/\(branch) has merge commits that \(branch) lacks: land them by hand (git log \(branch)..\(theirs))")
        }
        // By content: a commit landed before under another hash is not one of them.
        let picks = ProjectGit.cherryPicks(try await git(["cherry", branch, sha] + (limit.isEmpty ? [] : [limit])))
        if picks.isEmpty {
            if await gitSucceeds(["merge-base", "--is-ancestor", sha, branch]) {
                say("\(remote)/\(branch) has nothing \(branch) lacks")
                return
            }
            // Landed before, under other hashes: give the guest the local ones.
            return try await pushBack(branch, sha)
        }
        if let status = try? await git(["status", "--porcelain", "--untracked-files=no"]), !status.isEmpty {
            throw Failure("\(dir) has changes not committed: commit or stash them first")
        }
        let log = (try? await git(["log", "--no-walk=unsorted", "--format=%h %s (%an)"] + picks)) ?? ""
        let stat = (try? await git(["show", "--format=", "--stat=100"] + picks)) ?? ""
        let indent = { (text: String) in text.split(separator: "\n").map { "  " + $0 }.joined(separator: "\n") }
        say("\(remote)/\(branch) has \(picks.count) commit\(picks.count == 1 ? "" : "s") for \(branch):\n\(indent(log))"
            + (stat.isEmpty ? "" : "\n" + indent(String(stat.split(separator: "\n").last ?? ""))))
        say("review them first: git log -p \(branch)..\(theirs)")
        guard try await confirm("make them again on \(branch) as your commits? [y/N] ") else { throw Failure("not landed") }
        let tip = try await git(["rev-parse", "HEAD"])
        try await git(["update-ref", landBase(branch), tip])
        try await git(["update-ref", pending, sha])
        // cherry-pick keeps each author, makes the owner the committer and signs as commit.gpgsign says; one already
        // applied is dropped.
        do { try await git(["cherry-pick", "--empty=drop"] + picks) } catch {
            if await pickInProgress() {
                throw Failure("\(error)\nthe cherry-pick stopped: fix that in \(dir) and git cherry-pick --continue (or --abort), then glov vm land \(vm) again to give the guest the branch")
            }
            await dropLandRefs(branch)
            throw error
        }
        try await pushBack(branch, sha)
    }

    /// A cherry-pick stopped at a conflict.
    func pickInProgress() async -> Bool { await gitSucceeds(["rev-parse", "--verify", "-q", "CHERRY_PICK_HEAD"]) }

    private func dropLandRefs(_ branch: String) async {
        _ = try? await git(["update-ref", "-d", landRef(branch)])
        _ = try? await git(["update-ref", "-d", landBase(branch)])
    }

    /// Gives the guest the local branch, the landed commits in place of its own, unless its branch moved on since glov
    /// fetched `sha`.
    private func pushBack(_ branch: String, _ sha: String) async throws {
        let ref = "refs/heads/" + branch
        var refused: Error?
        do { try await git(["push", "-q", "--no-verify", "--force-with-lease=\(ref):\(sha)", remote, "\(ref):\(ref)"]) } catch { refused = error }
        await dropLandRefs(branch)
        if let refused {
            _ = try? await git(["update-ref", landedRef(branch), sha])
            throw Failure("\(branch) is landed here, but the guest did not take it back (it committed since, or has changes not committed): glov vm land \(vm) again lands what it added\n\(refused)")
        }
        _ = try? await git(["update-ref", "-d", landedRef(branch)])
        await fetch(branch: nil)
        say("\(branch) is yours now, here and in \(vm): the agent may push it and open the pull request (new commits there need another glov vm land)")
    }

    /// Per guest branch fetched, how many of its commits no local branch has, by content, against the local branch of the
    /// same name (else HEAD): what `glov vm delete` would lose. Only what was fetched; empty without the remote.
    public func unlanded() async -> [String] {
        guard let refs = try? await git(["for-each-ref", "--format=%(refname:short)", "refs/remotes/\(remote)/"]) else { return [] }
        var lines: [String] = []
        for ref in refs.split(whereSeparator: \.isWhitespace).map(String.init) {
            guard ref.hasPrefix(remote + "/") else { continue }
            var base = String(ref.dropFirst(remote.count + 1))
            if base == "HEAD" { continue }  // the remote's HEAD names one of its branches
            if await !gitSucceeds(["rev-parse", "--verify", "-q", "refs/heads/" + base]) { base = "HEAD" }
            let n = ProjectGit.cherryPicks((try? await git(["cherry", base, ref])) ?? "").count
            if n > 0 { lines.append("\(ref): \(n) commit\(n == 1 ? "" : "s") \(base) lacks") }
        }
        return lines
    }

    /// Removes the VM's remote (its remote-tracking branches with it) and `glov vm land`'s state from the project, and
    /// its host from glov's ssh_config: what `glov vm delete` leaves behind otherwise.
    public func forget() async {
        _ = try? await git(["remote", "remove", remote])
        if let refs = try? await git(["for-each-ref", "--format=%(refname)", "refs/glov-land/\(vm)/"]) {
            for ref in refs.split(whereSeparator: \.isWhitespace) { _ = try? await git(["update-ref", "-d", String(ref)]) }
        }
        try? editSSHConfig { ProjectGit.withoutSSHHost($0, vm: vm) }
    }

    /// Puts VM `vm`'s host in glov's ssh_config.
    public func writeSSHHost(config: VMConfig) throws {
        let glov = Bundle.main.executableURL?.resolvingSymlinksInPath().path ?? CommandLine.arguments[0]
        guard case .relay(var relay, _) = try SSHConnection.route(config: config, glov: glov) else {
            throw Failure("\(vm) is not a VZ VM; git mode reaches the guest through glov ssh-relay")
        }
        if let custom = ProcessInfo.processInfo.environment["GLOV_HOME"] {
            relay = "env GLOV_HOME=" + ProjectAgent.shellQuote(custom).replacingOccurrences(of: "%", with: "%%") + " " + relay
        }
        let host = ProjectGit.SSHHost(
            vm: vm, key: SSHConnection.keyPath(directory: config.directory), knownHosts: SSHConnection.knownHostsPath(directory: config.directory),
            relay: relay)
        try editSSHConfig { ProjectGit.withSSHHost($0, host) }
    }

    /// Rewrites glov's ssh_config with `change`, holding `ssh_config.lock`: glov runs for two projects at once would
    /// otherwise each write back what it read, dropping the other's VM.
    func editSSHConfig(_ change: (String) -> String) throws {
        let lock = open(sshConfigPath + ".lock", O_RDWR | O_CREAT | O_CLOEXEC, 0o600)
        guard lock >= 0 else { throw Failure("opening \(sshConfigPath).lock: \(String(cString: strerror(errno)))") }
        defer { close(lock) }  // closing releases the lock
        while flock(lock, LOCK_EX) != 0 {
            guard errno == EINTR else { throw Failure("locking \(sshConfigPath).lock: \(String(cString: strerror(errno)))") }
        }
        let old = (try? String(contentsOfFile: sshConfigPath, encoding: .utf8)) ?? ""
        let text = change(old)
        if text != old { try Self.writePrivate(text, to: sshConfigPath) }
    }

    /// Asks once (a no is remembered in ssh_config.declined) to include glov's ssh_config from ~/.ssh/config, so a
    /// manual `git fetch glov-<vm>` works too. glov's own git calls do without it.
    public func offerInclude() {
        let user = NSHomeDirectory()
        let file = user + "/.ssh/config"
        let text = (try? String(contentsOfFile: file, encoding: .utf8)) ?? ""
        let declined = sshConfigPath + ".declined"
        guard isatty(STDIN_FILENO) == 1, !ProjectGit.hasInclude(text, path: sshConfigPath, home: user),
              !FileManager.default.fileExists(atPath: declined) else { return }
        say("git fetch \(ProjectGit.remote(vm: "<vm>")) works by hand once \(file) has the line\n  \(ProjectGit.includeLine(path: sshConfigPath, home: user))")
        FileHandle.standardError.write(Data("add it at the top? [Y/n] ".utf8))
        guard ["", "y", "yes"].contains((readLine() ?? "n").trimmingCharacters(in: .whitespaces).lowercased()) else {
            try? Self.writePrivate("", to: declined)
            say("not added; glov still pushes and fetches itself")
            return
        }
        do {
            try FileManager.default.createDirectory(atPath: user + "/.ssh", withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            // Through a link (a dotfile manager's), so the file it points at changes, not the link.
            let target = (try? FileManager.default.destinationOfSymbolicLink(atPath: file)).map {
                $0.hasPrefix("/") ? $0 : (file as NSString).deletingLastPathComponent + "/" + $0
            } ?? file
            try Self.writePrivate(ProjectGit.withInclude(text, path: sshConfigPath, home: user), to: target)
        } catch { say("warning: \(file) was not changed: \(error)") }
    }

    static func writePrivate(_ text: String, to path: String) throws {
        try Data(text.utf8).write(to: URL(fileURLWithPath: path), options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path)
    }

    private func say(_ line: String) { log(line) }

    /// git on PATH.
    static func gitPath() throws -> String {
        for dir in (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":") where FileManager.default.isExecutableFile(atPath: "\(dir)/git") {
            return "\(dir)/git"
        }
        throw Failure("git is not on PATH")
    }
}
