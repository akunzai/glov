import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

/// `glov agent <harness>`, also spelled `glov claude`, `glov codex` or `glov agy` (main.swift): the harness on the current
/// project, in an isolated VM of the project's own.
struct AgentCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "agent", abstract: "Open a coding agent on the current project, in an isolated VM of its own.",
        discussion: """
            Also run as glov claude, glov codex or glov agy. The project is the git root of the current directory (the \
            directory itself outside git); its VM is named after it. The first run creates an isolated VM with the harness, \
            grants the secrets of the harness you have stored (glov secret set) and installs the harness; later runs start the VM if it is stopped, and add a harness it does not \
            have yet (glov vm harness add). The harness runs in a tmux \
            session as the guest user in the project directory: detach with Ctrl-b d and it keeps running; run the same \
            command again to attach. When the harness exits the session ends; glov claude -c continues its last \
            conversation. --new starts another session. Other arguments go to the harness (after -- if one is --new).

            A new VM may reach the repositories of the project's git remotes: glov lists them, with the git secrets of \
            their hosts, and asks; Enter accepts only when you confirmed each of them for this project before (a new remote may \
            be a guest's). Without a terminal, --allow-remotes applies them.

            --herdr runs the harness in herdr-core instead of tmux: in herdr-app, in a space of the VM's machine; elsewhere, \
            attached here with hc (detach with ctrl+\\). It makes the VM a herdr-core machine, which herdr-core trusts: glov \
            prints what that lets a hostile guest do, and asks the first time.

            The project reaches the VM in one of two ways, chosen when the VM is created and kept for its life. git (the \
            default): each run pushes the current branch to a repository at the same path on the guest's own disk, and \
            fetches the guest's branches back as refs/remotes/glov-<vm>/* when the harness exits or you detach (glov vm \
            fetch does it on demand, and after --herdr in herdr-app, where nothing returns); the guest commits unsigned and can never write to this Mac's repository. live \
            (--sync live, or picked at the first run's prompt): the working tree is shared read-write at the same path, \
            .git included, so hooks, git config, .envrc and build scripts the guest changes run on this Mac the next time \
            you use them there. A directory outside git can only be shared live. Git reaches the guest as the SSH host \
            glov-<vm>, written in ssh_config in the glov home; glov offers once to include it from ~/.ssh/config.
            """)
    @Argument(help: "Harness: \(HarnessProfile.all.map(\.command).joined(separator: ", ")) (or a profile name).") var harness: String
    @Flag(help: "Start another session instead of attaching the harness's own.") var new = false
    @Option(help: "On a new VM: git (the default: the project reaches the guest as commits, in a repository on its disk) or live (the folder itself is shared read-write, .git included). A VM keeps its mode.")
    var sync: VMConfig.Project.Sync?
    @Flag(help: "On a new VM, let it reach the repositories of the project's git remotes without asking (needed without a terminal).")
    var allowRemotes = false
    @Flag(help: "Run the harness in herdr: the VM becomes a herdr-core machine (glov warns what that lets the guest do) and herdr-app shows it in a space of that machine, or this terminal attaches with hc.")
    var herdr = false
    @Option(help: "On a new VM: restricted (the default: only the allowlist) or open (any public host, still only through the egress proxy). For an existing VM, use glov vm configure <vm> --egress.")
    var egress: EgressModeOption?
    @Flag(help: "With --egress open: grant the harness's stored secrets even when it takes one as a real value in tmpfs, which any host could then receive (needed without a terminal; with one, glov asks).")
    var allowPlainSecrets = false
    @Flag(help: "With --herdr, add the VM as a herdr-core machine without asking (needed without a terminal).") var yes = false
    /// What glov does not take is the harness's: `glov claude -c` is `glov claude -- -c`.
    @Argument(parsing: .allUnrecognized, help: "Arguments for the harness (after -- when one is also a glov option).") var args: [String] = []

    static let readyTimeout: Double = 300

    func run() async throws {
        let next: Next, back: FetchBack?
        do { (next, back) = try await prepare() } catch let e as ExitCode { throw e } catch {
            FileHandle.standardError.write(Data("glov: \(error is CocoaError ? error.localizedDescription : "\(error)")\n".utf8))
            throw ExitCode.failure
        }
        let path: String, argv: [String]
        switch next {
        case .ssh(let a): (path, argv) = ("/usr/bin/ssh", a)
        case .exec(let p, let a): (path, argv) = (p, a)
        case .done: return
        }
        guard let back else { try SSH.replaceProcess(with: argv, path: path) }
        // Git mode fetches the guest's commits once the session returns (the harness exited, or a detach), so glov waits.
        let code = try SSH.runChild(argv, path: path)
        await back.repo.fetch(branch: back.branch)
        if code != 0 { throw ExitCode(code) }
    }

    /// What `run` does once the VM is ready: ssh into its tmux session, become another program (`hc attach`), or nothing.
    enum Next { case ssh([String]), exec(String, [String]), done }

    /// In git mode, what to fetch from once the session returns.
    struct FetchBack {
        let repo: ProjectRepo
        let branch: String?
    }

    private func prepare() async throws -> (Next, FetchBack?) {
        guard let profile = ProjectAgent.harness(for: harness) else {
            throw Failure("unknown harness \(harness): use \(HarnessProfile.all.map(\.command).joined(separator: ", "))")
        }
        let home = GlovHome.resolve()
        let project = ProjectAgent.root(of: FileManager.default.currentDirectoryPath)
        let name = ProjectAgent.vmName(project: project)
        let client = daemonClient()
        say("\(project): VM \(name)")

        let mode: VMConfig.Project.Sync
        if home.vmNames().contains(name) {
            let config = try home.loadConfig(name)
            guard let existing = config.projectSync(path: project) else {
                throw Failure("VM \(name) is not \(project)'s VM")
            }
            mode = existing
            if let sync, sync != mode {
                throw Failure("VM \(name) reaches \(project) \(mode == .git ? "through git" : "live"), and a VM keeps its mode: glov vm delete \(name) first to change it")
            }
            if mode == .git && !Self.isRepository(project) {
                throw Failure("\(project) is in no git repository any more, and VM \(name) reaches it through git")
            }
            if mode == .live { say("reminder: \(name) shares \(project) live; " + ProjectGit.liveWarning.dropFirst("warning: ".count)) }
            if let egress, (config.egress?.open == true) != (egress == .open) {
                throw Failure("VM \(name) already exists with egress \(config.egress?.open == true ? "open" : "restricted"): change it with glov vm configure \(name) --egress \(egress.rawValue)")
            }
        } else {
            mode = try chooseSync(project)
            try await create(name, project: project, profile: profile, mode: mode, home: home, client: client)
        }

        let info = try await client.call(Method.vmStatus, Params.VM(vm: name), as: Results.VMInfo.self)
        if info.state != "running" {
            say("starting \(name)")
            _ = try await client.call(Method.vmStart, Params.VM(vm: name), as: Results.VMInfo.self)
        } else if let agent = info.agent, agent.state == "unresponsive" {
            // Waiting would only time out: a guest that answered before and went silent is hung or rebooting.
            throw Failure("\(name): the Guest Service is not answering (\(agent.problem ?? "unresponsive")); the guest may be hung: "
                + "glov vm screenshot \(name) shows its console; glov vm stop --force \(name), then glov \(profile.command) again")
        }
        if info.agent?.state != "ready" {
            say("waiting for \(name)'s Guest Service (up to \(Int(Self.readyTimeout / 60)) minutes; the first boot takes longest)")
            _ = try await client.call(Method.vmWait, Params.Wait(vm: name, state: "guest-ready", timeout: Self.readyTimeout), as: Results.VMInfo.self)
        }
        if try home.loadConfig(name).harnesses?.contains(profile.name) != true {
            // A project's VM takes each harness the first time it is asked for.
            try await HarnessCommand.Add.parse([name, profile.name]).run()
            try await grantStoredSecrets(name, profile: profile, client: client)
        } else if try await run(client, name, ["test", "-x", "/usr/local/bin/" + profile.command], as: "system").code != 0 {
            try await HarnessCommand.Install.parse([name]).run()
        }
        say(mode == .live ? "waiting for the console sign-in and the project share" : "waiting for the console sign-in")
        try await waitForUser(client, name)
        var back: FetchBack?
        if mode == .live {
            try await waitForShare(client, name)
        } else {
            let repo = ProjectRepo(dir: project, vm: name, home: home)
            let guestRun: ProjectRepo.GuestRun = { argv, runAs in try await self.run(client, name, argv, as: runAs) }
            try await repo.connect(config: try home.loadConfig(name), guest: project, run: guestRun)
            back = FetchBack(repo: repo, branch: await repo.push(guest: project, run: guestRun))
        }
        if herdr { return (try await herdrSession(name, project: project, profile: profile), back) }
        guard try await run(client, name, ["sh", "-c", "command -v tmux"], as: "user").code == 0 else {
            throw Failure("\(name) has no tmux; glov \(profile.command) runs the harness in a tmux session")
        }
        let listed = try await run(client, name, ["sh", "-c", "tmux -L \"$1\" list-sessions -F '#{session_name}' 2>/dev/null || true", "sh", profile.name], as: "user")
        let existing = Set(listed.tail.split(whereSeparator: \.isNewline).map(String.init))
        let session = ProjectAgent.session(harness: profile.name, existing: existing, new: new)
        if existing.contains(session) && !ProjectAgent.harnessArguments(args).isEmpty {
            say("attaching the running session \(session); its harness keeps the arguments it started with (--new starts another)")
        }
        say(existing.contains(session) ? "attaching \(session)" : "starting \(session) (detach with Ctrl-b d)")
        let command = ProjectAgent.tmuxCommand(server: profile.name, session: session, directory: project, argv: [profile.command] + ProjectAgent.harnessArguments(args))
        return (.ssh(try await SSH.argv(vm: name, extra: ["-t", command])), back)
    }

    /// Whether `project` (a `ProjectAgent.root`) is a git repository's root, a worktree's included.
    static func isRepository(_ project: String) -> Bool { FileManager.default.fileExists(atPath: project + "/.git") }

    /// How a new VM reaches `project`: `--sync`, else git for a repository (the first run's prompt, in a terminal, may
    /// pick live). A directory outside git can only be shared live, which the owner must pick.
    private func chooseSync(_ project: String) throws -> VMConfig.Project.Sync {
        let repository = Self.isRepository(project)
        let terminal = isatty(STDIN_FILENO) == 1
        var mode: VMConfig.Project.Sync
        if let sync {
            guard sync == .live || repository else { throw Failure("\(project) is no git repository: git init and commit first, or --sync live") }
            mode = sync
        } else if !repository {
            guard terminal else { throw Failure("glov reaches the VM through git (git init and commit first in \(project)), or --sync live shares the folder") }
            FileHandle.standardError.write(Data("\(project) is no git repository; share the folder live instead? [y/N] ".utf8))
            guard ["y", "yes"].contains((readLine() ?? "").trimmingCharacters(in: .whitespaces).lowercased()) else {
                throw Failure("not created; git init and commit first, or --sync live")
            }
            mode = .live
        } else if terminal {
            say("how should the VM reach \(project)?")
            say("  git: your commits go to a repository on the VM's disk and its commits come back to review (the default)")
            say("  live: the folder itself, .git included, is shared read-write")
            FileHandle.standardError.write(Data("[git/live] ".utf8))
            let answer = readLine() ?? ""
            guard let picked = ProjectGit.choice(answer) else { throw Failure("not created: \"\(answer)\" is neither git nor live") }
            mode = picked
        } else {
            mode = .git
        }
        if mode == .live { say(ProjectGit.liveWarning) }
        return mode
    }

    /// The harness in herdr-core on machine `name` (`HerdrView`): added as a machine once the owner agrees, then shown in
    /// herdr-app's space for it, or attached here.
    private func herdrSession(_ name: String, project: String, profile: HarnessProfile) async throws -> Next {
        guard let hc = try? SecretCommand.Source.resolve("hc") else {
            throw Failure("--herdr needs herdr-core's hc on PATH (https://herdr.dev)")
        }
        say(HerdrView.warning)
        let machines = HerdrView.machines(try tool(hc, ["machine", "ls", "--json"]))
        if !machines.contains(where: { HerdrView.isVM(name: $0.name, target: $0.target, vm: name) }) {
            if machines.contains(where: { $0.name == name }) {
                throw Failure("herdr-core already has a machine named \(name) that is not reached through glov ssh; hc machine rm \(name) first")
            }
            if !yes {
                guard isatty(STDIN_FILENO) == 1 else { throw Failure("adding \(name) as a herdr-core machine needs your yes: run in a terminal, or with --yes") }
                FileHandle.standardError.write(Data("add \(name) as a herdr-core machine, with the risks above? [y/N] ".utf8))
                guard ["y", "yes"].contains((readLine() ?? "").trimmingCharacters(in: .whitespaces).lowercased()) else {
                    throw Failure("not added; glov \(profile.command) without --herdr uses tmux")
                }
            }
            let glov = Bundle.main.executableURL?.resolvingSymlinksInPath().path ?? CommandLine.arguments[0]
            let via = HerdrView.via(glov: glov, home: ProcessInfo.processInfo.environment["GLOV_HOME"], vm: name)
            say("adding \(name) as a herdr-core machine (hc is copied into the VM)")
            _ = try tool(hc, ["machine", "add", "--via", via, "--name", name, "--install"])
        }
        let argv = [profile.command] + ProjectAgent.harnessArguments(args)
        if ProcessInfo.processInfo.environment["HC_TERMINAL_ID"] != nil, let app = try? SecretCommand.Source.resolve("herdr-app"),
           let ls = try? tool(app, ["ls"]) {
            let found = HerdrView.place(ls, vm: name, folder: project, tab: profile.name)
            let session = ProjectAgent.session(harness: profile.name, existing: found.tabs, new: new)
            var stale: String?
            if !new, let terminal = found.terminal {
                let now = await occupant(hc, terminal, profile)
                if now == .shell {
                    say(HerdrView.restarting(session, vm: name, command: profile.command))
                    stale = found.tab
                } else {
                    if case .other(let program) = now {
                        say(HerdrView.leftAlone(session, vm: name, program: program, command: profile.command))
                    } else {
                        say("showing the running session \(session) in herdr")
                    }
                    _ = try tool(app, ["do", "agent.focus", try json(["terminal": terminal])])
                    return .done
                }
            }
            if let space = found.space {
                say("starting \(session) in herdr's space for \(name)")
                _ = try tool(app, ["do", "tab.new", try json(["space": space, "name": session, "folder": project, "argv": argv])])
                // After the new tab: closing a space's last tab closes the space.
                if let stale { _ = try tool(app, ["do", "tab.close", try json(["id": stale, "confirmed": true])]) }
            } else {
                say("starting \(session) in a new herdr space for \(name)")
                let title = (project as NSString).lastPathComponent + " (vm)"
                let request = try json(["name": title, "folder": project, "machine": name, "tab": session, "argv": argv])
                // herdr-app hears of a machine just added a little later.
                let deadline = Date().addingTimeInterval(30)
                while true {
                    do { _ = try tool(app, ["do", "space.new", request]); break } catch {
                        guard "\(error)".contains("no machine called"), Date() < deadline else { throw error }
                        try await Task.sleep(for: .seconds(1))
                    }
                }
            }
            return .done
        }
        let existing = HerdrView.liveNames(try tool(hc, ["ls", "--json"]), vm: name)
        let session = ProjectAgent.session(harness: profile.name, existing: existing, new: new)
        var running = existing.contains(session)
        if running {
            switch await occupant(hc, "\(name)/\(session)", profile) {
            case .harness: break
            case .shell:
                say(HerdrView.restarting(session, vm: name, command: profile.command))
                _ = try tool(hc, ["kill", "\(name)/\(session)"])
                _ = try? tool(hc, ["rm", "\(name)/\(session)"])
                running = false
            case .other(let program):
                say(HerdrView.leftAlone(session, vm: name, program: program, command: profile.command))
            }
        }
        if running {
            say("attaching \(name)/\(session) (detach with ctrl+\\)")
        } else {
            say("starting \(name)/\(session) (detach with ctrl+\\)")
            // No parent: a terminal in the VM may only reach the Mac terminals that started its own.
            _ = try tool(hc, ["run", "--on", name, "--no-parent", "--keep", "--bg", "--name", session, "--cwd", project, "--"] + argv)
        }
        return .exec(hc, ["hc", "attach", "\(name)/\(session)"])
    }

    private func json(_ object: [String: Any]) throws -> String {
        String(decoding: try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes]), as: UTF8.self)
    }

    private func tool(_ path: String, _ arguments: [String]) throws -> Data { try HerdrTool.run(path, arguments) }

    /// What `terminal` runs now (`HerdrView.occupant`), once hc has restored it after a VM restart (about 2 s after the
    /// machine connects; glov waits up to 15 s); the harness when hc cannot say.
    private func occupant(_ hc: String, _ terminal: String, _ profile: HarnessProfile) async -> HerdrView.Occupant {
        let deadline = ContinuousClock.now + .seconds(15)
        while true {
            guard let json = try? tool(hc, ["get", terminal, "--json"]) else { return .harness }
            if !HerdrView.restoring(json) || ContinuousClock.now >= deadline { return HerdrView.occupant(json, command: profile.command) }
            guard (try? await Task.sleep(for: .milliseconds(500))) != nil else { return .harness }
        }
    }

    private func create(_ name: String, project: String, profile: HarnessProfile, mode: VMConfig.Project.Sync, home: GlovHome, client: DaemonClient) async throws {
        let share = mode == .live ? try ProjectShare.samePath(project, name: ProjectAgent.shareName, home: home) : nil
        try await VMCommand.Create.parse([name, "--engine", "vz", "--isolated", "--no-start", "--harness", profile.name] + (egress.map { ["--egress", $0.rawValue] } ?? [])).run()
        _ = try home.update(name) { config in config.project = .init(path: project, sync: mode) }
        if let share {
            // Same path in the guest, which `glov share add` refuses (/Users is not a mount root).
            _ = try home.update(name) { config in config = try config.adding(share) }
            say("shared \(project) read-write at the same path in \(name)")
            if let git = ProjectAgent.gitDirectory(project: project) {
                // A worktree's .git names its repository's; without it git fails in the guest.
                let repo = try ProjectShare.samePath(git, name: ProjectAgent.gitShareName, home: home)
                _ = try home.update(name) { config in config = try config.adding(repo) }
                say("shared its repository \(git) read-write at the same path too (a worktree's git data lives there)")
            }
        } else {
            say("\(name) gets \(project) through git: a repository at the same path on its own disk")
        }
        try await grantStoredSecrets(name, profile: profile, client: client)
        try await allowProjectRepos(name, project: project, home: home, client: client)
    }

    /// Lets the VM reach the repositories of the project's remotes, grants the git secrets of their hosts and allows
    /// those hosts, once the owner confirms (`ProjectRemotes`).
    private func allowProjectRepos(_ name: String, project: String, home: GlovHome, client: DaemonClient) async throws {
        let repos = ProjectAgent.remoteRepos(project: project)
        guard !repos.isEmpty else { return }
        let store = ProjectRemotes(home: home.path)
        let known = store.confirmed(project)
        let hosts = Set(repos.map(\.host))
        let git = try await client.call(Method.secretList, Params.Secret(), as: Results.Secrets.self).secrets.filter { s in
            guard let f = s.forge.flatMap(Forge.init(rawValue:)) else { return false }
            return !hosts.isDisjoint(with: GitSignIn.Entry(name: s.name, forge: f, hosts: s.hosts).gitHosts)
        }
        let allow = git.flatMap(\.hosts).reduce(into: [String]()) { if !$0.contains($1) { $0.append($1) } }
        let anyNew = repos.contains { !known.contains($0.text) }
        say("the project's git remotes name\(anyNew ? " (new: not confirmed for this project before; a guest may have added it)" : ""):")
        for r in repos { say("  \(known.contains(r.text) ? "" : "new: ")\(r.text)") }
        if !git.isEmpty { say("  git sign-in \(git.map(\.name).joined(separator: ", ")), allowing \(allow.joined(separator: ", "))") }
        let terminal = isatty(STDIN_FILENO) == 1
        let apply = ProjectRemotes.apply(anyNew: anyNew, isTerminal: terminal, allowFlag: allowRemotes) {
            FileHandle.standardError.write(Data("let \(name) reach them? [\(anyNew ? "y/N" : "Y/n")] ".utf8))
            return readLine()
        }
        guard apply else {
            say(terminal ? "not applied; glov secret repos \(name) add <repo> and glov secret grant \(name) <secret> do it by hand"
                : "not applied without a terminal; run with --allow-remotes, or glov secret repos \(name) add <repo> by hand")
            return
        }
        if allowRemotes { for r in repos where !known.contains(r.text) { say("warning: applying new remote \(r.text)") } }
        _ = try await client.call(Method.secretRepos, Params.SecretRepos(vm: name, add: repos.map(\.text)), as: Results.SecretRepos.self)
        if !git.isEmpty {
            _ = try await client.call(Method.secretGrant, Params.SecretGrant(vm: name, names: git.map(\.name)), as: Results.SecretGrants.self)
            _ = try await client.call(Method.egressAllow, Params.Egress(vm: name, hosts: allow), as: Results.Egress.self)
        }
        try store.confirm(project, repos)
        say("\(name) may reach \(repos.map(\.text).joined(separator: ", ")) (glov secret repos \(name) to change)")
    }

    /// Grants the profile's secrets that the owner has stored, and says which are not.
    private func grantStoredSecrets(_ name: String, profile: HarnessProfile, client: DaemonClient) async throws {
        let defined = Set(try await client.call(Method.secretList, Params.Secret(), as: Results.Secrets.self).secrets.map(\.name))
        let grant = profile.secrets.map(\.name).filter(defined.contains)
        if !grant.isEmpty {
            _ = try await confirmingPlainSecrets(allow: allowPlainSecrets) { allow in
                try await client.call(Method.secretGrant, Params.SecretGrant(vm: name, names: grant, allowPlainSecrets: allow ? true : nil), as: Results.SecretGrants.self)
            }
            say("granted \(grant.joined(separator: ", ")) to \(name)")
        }
        let missing = profile.secrets.map(\.name).filter { !defined.contains($0) }
        if !missing.isEmpty {
            say("not stored yet: \(missing.joined(separator: ", ")); glov secret set <name> --host <host>, then glov secret grant \(name) <name> "
                + "(or glov secret login \(name) \(profile.name) for a subscription)")
        }
    }

    /// `exec --as user` exits 75 until the console sign-in, a few seconds after the Guest Service is ready.
    private func waitForUser(_ client: DaemonClient, _ vm: String) async throws {
        let deadline = Date().addingTimeInterval(HarnessCommand.Install.signInTimeout.seconds)
        while try await run(client, vm, ["true"], as: "user").code != 0 {
            guard Date() < deadline else { throw Failure("\(vm): the guest user is not signed in on the console yet; try again shortly") }
            try await Task.sleep(for: .seconds(2))
        }
    }

    private func waitForShare(_ client: DaemonClient, _ vm: String) async throws {
        let deadline = Date().addingTimeInterval(60)
        while true {
            let info = try await client.call(Method.vmStatus, Params.VM(vm: vm), as: Results.VMInfo.self)
            let mount = info.agent?.shares?.first { $0.name == ProjectAgent.shareName }
            if mount?.state == "mounted" { return }
            if mount?.state == "failed" { throw Failure("\(vm): the project share did not mount: \(mount?.problem ?? "unknown")") }
            guard Date() < deadline else { throw Failure("\(vm): the project share is not mounted yet; glov vm status \(vm) shows it") }
            try await Task.sleep(for: .seconds(1))
        }
    }

    private func run(_ client: DaemonClient, _ vm: String, _ argv: [String], as runAs: String) async throws -> (code: Int, tail: String) {
        let r = try await client.call(Method.execRun, Params.Exec(vm: vm, argv: argv, as: runAs, timeout: 30), as: Results.ExecRun.self)
        return (r.exited ? r.code ?? -1 : -1, r.tail)
    }

    private func say(_ line: String) { FileHandle.standardError.write(Data((line + "\n").utf8)) }

    struct Failure: Error, CustomStringConvertible {
        let description: String
        init(_ d: String) { description = d }
    }
}

extension Duration {
    fileprivate var seconds: Double { Double(components.seconds) + Double(components.attoseconds) / 1e18 }
}
