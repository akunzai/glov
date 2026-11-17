import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

extension VMCommand {
    /// `glov vm fetch`: a git-mode project VM's branches into its project, on demand.
    struct Fetch: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Fetch a project VM's branches into the project as refs/remotes/glov-<vm>/* (git mode, glov claude).",
            discussion: """
                glov claude does it when the harness exits or you detach; this is for a session still running. Fetching runs no \
                hook and copies no config of the guest's. Review what came with git log <branch>..glov-<vm>/<branch>.
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String

        func run() async throws {
            try await report {
                let (repo, _) = try await ProjectVM.running(vm)
                guard await repo.fetch(branch: await repo.currentBranch()) else { throw ExitCode.failure }
                print("fetched \(repo.vm)'s branches into \(repo.dir) as refs/remotes/\(repo.remote)/*")
            }
        }
    }
}

extension VMCommand {
    /// `glov vm land`: the guest's commits made the owner's, signed on this Mac, and given back to the guest.
    struct Land: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Make a project VM's new commits on the checked-out branch yours, signed here, and give the branch back (git mode).",
            discussion: """
                Fetches the guest's branches, lists the guest's commits that the checked-out branch lacks (by content) for you \
                to review, and once you agree (--yes without a terminal) makes each again with git cherry-pick: each keeps its \
                author, you are the committer, and it is signed as your git config signs (commit.gpgsign), with your key, \
                which never enters the guest. Then it gives the guest the branch (--force-with-lease against what it \
                fetched), so the agent can push it and open a pull request with your signatures intact. The agent must not \
                amend or rebase landed commits; new ones need another land. A commit that does not apply stops the \
                cherry-pick for you: resolve it and git cherry-pick --continue (or --abort), then run glov vm land again, \
                which only gives the branch back. A guest that committed meanwhile keeps its branch; the next land takes \
                only what it added. Merge commits from the guest are landed by hand.
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "The branch to land: the checked-out one (the default).") var branch: String?
        @Flag(name: .shortAndLong, help: "Land without asking, once you reviewed the commits (needed without a terminal).") var yes = false

        func run() async throws {
            try await report {
                let (repo, _) = try await ProjectVM.running(vm)
                try await repo.land(branch: branch) { question in
                    if yes { return true }
                    guard isatty(STDIN_FILENO) == 1 else {
                        throw ValidationError("landing them needs your yes: run in a terminal, or with --yes once you reviewed them")
                    }
                    FileHandle.standardError.write(Data(question.utf8))
                    return ["y", "yes"].contains((readLine() ?? "").trimmingCharacters(in: .whitespaces).lowercased())
                }
            }
        }
    }
}

/// A git-mode project VM (`VMConfig.Project`), as the commands that reach its guest repository need it.
enum ProjectVM {
    /// The project repository of running VM `reference`, with its SSH host written afresh (the relay names the glov
    /// that runs now, which an upgrade may have moved).
    static func running(_ reference: String) async throws -> (ProjectRepo, VMConfig) {
        let home = GlovHome.resolve()
        let info = try await daemonClient().call(Method.vmStatus, Params.VM(vm: reference), as: Results.VMInfo.self)
        let config = try home.loadConfig(info.name)
        guard let project = config.project, project.sync == .git else {
            throw ValidationError("VM \(info.name) is not a project's VM reached through git (glov claude)")
        }
        guard info.state == "running" else { throw ValidationError("VM \(info.name) is \(info.state): glov vm start \(info.name) first") }
        let repo = ProjectRepo(dir: project.path, vm: info.name, home: home)
        try repo.writeSSHHost(config: config)
        return (repo, config)
    }

    /// Before `glov vm stop`: fetches the guest's branches of a running git-mode VM, so `glov vm delete`, which needs
    /// the VM stopped, can list the guest's commits no local branch has, even those made after the session that started
    /// them returned (a herdr-app space never returns). A failure only warns: the VM still stops.
    static func fetchBeforeStop(_ reference: String) async {
        guard let info = try? await daemonClient().call(Method.vmStatus, Params.VM(vm: reference), as: Results.VMInfo.self),
            info.state == "running", (try? GlovHome.resolve().loadConfig(info.name))?.project?.sync == .git
        else { return }
        let failed = { (why: String) in say("warning: \(why); the guest's commits since the last fetch are not in the project: glov vm start \(info.name), then glov vm fetch \(info.name)") }
        guard info.agent?.state == "ready" else { return failed("\(info.name)'s Guest Service is not ready, so its branches were not fetched") }
        do {
            let (repo, _) = try await running(info.name)
            guard await repo.fetch(branch: await repo.currentBranch()) else { return failed("fetching from \(info.name) failed") }
            say("fetched \(info.name)'s branches into \(repo.dir)")
        } catch { failed("\(error)") }
    }

    /// Before `glov vm delete` of git-mode VM `name`: the guest's commits that no local branch has are listed and, unless
    /// the owner agrees (`yes`, or in a terminal), keep the VM. A stopped VM may hold commits made since the last fetch
    /// (one the guest or macOS shut down was not fetched by `glov vm stop`), so in a terminal glov offers to start it,
    /// fetch and stop it again. The project's repository to forget the VM in afterwards; nil for any other VM.
    static func checkBeforeDelete(_ name: String, yes: Bool) async throws -> ProjectRepo? {
        let home = GlovHome.resolve()
        guard let config = try? home.loadConfig(name), let project = config.project, project.sync == .git else { return nil }
        if try await VMDeletion.isRunning(name, in: home) {
            throw ValidationError("VM \(name) is running: glov vm stop \(name) first, which fetches its branches")
        }
        try await fetchStopped(name, yes: yes)
        let repo = ProjectRepo(dir: project.path, vm: name, home: home)
        let lines = await repo.unlanded()
        guard !lines.isEmpty else { return repo }
        say("\(repo.dir) has guest commits that are on no local branch, gone with the VM:\n" + lines.map { "  " + $0 }.joined(separator: "\n"))
        if !yes {
            guard isatty(STDIN_FILENO) == 1 else {
                throw ValidationError("not deleted; glov vm land or git merge brings them in (--yes deletes them with the VM)")
            }
            FileHandle.standardError.write(Data("delete them with the VM? [y/N] ".utf8))
            guard VMDeletion.isYes(readLine()) else { throw ValidationError("not deleted; glov vm land or git merge brings them in") }
        }
        return repo
    }

    /// Offers to start stopped VM `name` and fetch its branches; with `yes` or without a terminal it only warns. A start
    /// or fetch that fails keeps the VM.
    private static func fetchStopped(_ name: String, yes: Bool) async throws {
        let unknown = "the guest's commits since \(name)'s last fetch are not listed"
        guard !yes, isatty(STDIN_FILENO) == 1 else {
            return say("warning: \(unknown) (glov vm start \(name), then glov vm fetch \(name), lists them)")
        }
        say("\(name) is stopped, so \(unknown)")
        FileHandle.standardError.write(Data("start it to fetch them first? [Y/n] ".utf8))
        let answer = (readLine() ?? "n").trimmingCharacters(in: .whitespaces).lowercased()
        guard ["", "y", "yes"].contains(answer) else { return }
        let client = daemonClient()
        say("starting \(name)")
        var failure: Error?
        do {
            _ = try await client.call(Method.vmStart, Params.VM(vm: name), as: Results.VMInfo.self)
            _ = try await client.call(Method.vmWait, Params.Wait(vm: name, state: "guest-ready", timeout: AgentCommand.readyTimeout), as: Results.VMInfo.self)
            let (repo, _) = try await running(name)
            if await !repo.fetch(branch: await repo.currentBranch()) { failure = ValidationError("fetching from \(name) failed") }
        } catch { failure = error }
        say("stopping \(name)")
        do { _ = try await client.call(Method.vmStop, Params.Stop(vm: name, force: false), as: Results.VMInfo.self) } catch { failure = failure ?? error }
        if let failure { throw ValidationError("not deleted: \(failure) (glov vm delete \(name) --yes deletes it without the fetch)") }
        say("fetched \(name)'s branches into the project")
    }

    private static func say(_ line: String) { FileHandle.standardError.write(Data((line + "\n").utf8)) }
}
