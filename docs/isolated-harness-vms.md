# Run a coding agent in an isolated VM

An isolated Linux (VZ) VM runs a coding-agent harness (Claude Code, Codex or Antigravity CLI) unattended, on the
assumption that the harness may turn hostile, for example after a prompt injection. So the boundary is on the Mac, never
in the guest:

- The VM has no network device. Its only way out is the egress proxy in `glovd`, over vsock, which allows only the
  hosts in the VM's allowlist (any public host once you open it, see [Open network](#open-network)) and refuses the Mac, the LAN and private ranges even when they are listed, unless you
  allow a private-network host by name (`glov egress allow <vm> <host> --private`; its CA with `glov vm trust add`).
- The real model-API key stays in the Mac's Keychain as a secret. The guest holds a placeholder, and the proxy puts the
  key in its place on the way to the provider.
- The guest cannot call `glovd`'s control or MCP methods, so even root in the guest cannot widen its allowlist,
  or change its shares or snapshots.
- The project is a dedicated clone, never your working repo, so the guest cannot plant git hooks or config where the
  Mac runs git. Results come back as a `git bundle` that you fetch and review.

## Set up

```sh
glov secret set anthropic --host api.anthropic.com   # once: reads the key from stdin; also openai, gemini
glov vm create agent --engine vz --isolated --harness claude   # or codex, antigravity; repeatable
glov secret grant agent anthropic    # vm create left the VM running; a grant applies at once
glov vm harness install agent       # installs the harnesses, takes the install hosts away again, copies your config
```

`--harness` gives the harness the secrets its profile declares (the placeholder `glov-secret:<provider>` as its API
key), allows the hosts it needs at run time (the provider's API host among them), and sets permission defaults for
unattended runs; `vm create` lists the secrets still to define and grant. The install then copies your own instructions,
skills and agents from the Mac into the VM, links followed and credentials left out (`--no-host-config` skips that);
[docs/networking.md](networking.md#harness-profiles) lists what each harness copies.
Then make the VM a template: set the guest's git identity, stop it and `glov snapshot create agent ready`. Each task
reverts to `ready`, adds a fresh `git clone --no-local` of the project as a share at `/mnt/task`, starts the VM, runs the
harness with `glov exec agent --as user --cwd /mnt/task -- claude -p "..."`, commits and writes a bundle in the guest,
stops the VM and removes the share; you then `git fetch` the bundle into the working repo. The full command sequence is in
[docs/networking.md](networking.md#harness-template-and-tasks).

## On the current project

For interactive work, `glov claude` (or `glov codex`, `glov agy`) in a project directory opens that harness in an
isolated VM of the project's own:

```sh
cd ~/src/app
glov claude              # first run: creates, provisions and starts the VM; later runs reuse it
glov claude --new        # another session in the same VM
glov claude -c            # other arguments go to the harness: here, continue its last conversation
```

- The project is the git root of the current directory (the directory itself outside git), found without running git.
  The VM is named after its folder plus a hash of its path (`app-1a2b3c`). The project reaches it through git or live,
  as below; either way it is at the project's own path in the guest.
- The first run creates the VM with `--isolated --harness <name>`, grants the secrets of the harness that you have
  stored (`glov secret set`), says which are missing, and runs `glov vm harness install`. Later runs start the VM if it
  is stopped. The VM keeps its state between sessions: it is never reverted.
- The harness runs as the guest user in a tmux session named after it, in the project directory. Detach with Ctrl-b d
  (or close the terminal) and it keeps running; the same command attaches again. When the harness exits (`/exit`,
  Ctrl-C), the session ends, and the next run starts a new conversation: `glov claude -c` (or `/resume` inside)
  continues one, since the VM keeps them. Write `--` before harness arguments only when one is `--new`.
- One VM serves every harness of the project: the first `glov codex` in a project whose VM has only claude adds codex
  to it (`glov vm harness add`, below) and installs it. Each harness has a tmux server of its own, so the variables of
  one added later reach its sessions while another's keep running.
- `glov vm harness add <vm> <harness>` does the same for any running isolated VM made with `--harness` (one made
  without it is refused, since only `--harness` signs the guest user in): it writes the harness's environment, its
  home files (keeping one already there), its secret links and, for codex, the user-namespace setting, allows its
  runtime hosts, adds it to vm.json and installs only it (`glov vm harness install --harness <harness>`). Grant its
  secrets afterwards; `glov claude`, `codex` and `agy` grant the stored ones themselves.

### In herdr

`glov claude --herdr` runs the harness in herdr-core (`hc`) instead of tmux, so herdr shows it as an agent (working,
idle, blocked):

- The VM becomes a herdr-core machine named after it, reached through `glov ssh <vm> --` (`hc machine add --via ...
  --install`, which copies the Mac's hc into the guest; no allowlist change). The first time, glov asks (`--yes`
  without a terminal). `glov vm delete` removes the machine again.
- Run in a herdr-app pane, it opens the harness in a tab named after it (`claude`, `claude-2` with `--new`) of a space
  of the VM's machine, `<folder> (vm)`, or shows the running one. Elsewhere it starts the terminal in the guest
  (`hc run --on <vm> --no-parent --keep`) and attaches this terminal to it (`hc attach`, detach with ctrl+\).
- A session's terminal outlives a VM restart, but hc replaces the harness in it with a shell, and a herdr-app tab
  also drops to a shell when the harness exits. The next run finds that shell at its prompt, says so in one line and
  starts the harness again in its place (`glov claude -c` continues the conversation). A program someone started in
  that shell is shown as it is, never ended; quit it and run again to get the harness.
- **It widens what a hostile guest can reach on the Mac.** herdr-core trusts its machines: the guest's hc sends the
  Mac's core terminal statuses, labels and a layout document, not only a terminal stream. A guest can type text and
  Enter into any Mac terminal that started a terminal in the VM (`hc run --on <vm>` from a Mac shell or agent; glov's
  own sessions have no parent, and other Mac terminals are refused), make herdr-app open spaces and tabs and show
  statuses, sounds and toasts, write the Mac's clipboard (herdr-app's `clipboard_write: false` stops it), and claim
  it needs a login. glov prints this each time; herdr-core's `notes = false` stops its notes to parent agents.

### Git or live

A new VM asks how the project reaches it in a terminal (`git`, the default, or `live`); `--sync git|live` chooses
without asking, and without a terminal it is git. A VM keeps its mode: delete it to change the mode. A VM made before
the modes existed shares the project live, and each run reminds you of it. In git mode:

- The project must be a git repository. The VM's own repository is at the project's path on the guest's disk. It
  commits under your `user.name` and `user.email`, unsigned: your signing key stays on the Mac. Its `origin` is the
  project's `origin`, and the VM may reach that repository once you confirm (above).
- Each run fetches the guest's branches, then pushes the current branch if the guest lacks commits of it, and checks
  the guest out on it. Uncommitted changes on the Mac stay there, and glov says how many. A guest that is ahead keeps
  its commits; one whose branch went another way, or with changes not committed, keeps its state, and glov warns.
  Nothing is forced. In a worktree, its branch goes like any checkout's; nothing of the repository is shared.
  While a program works in the guest's copy (a harness session, or a command a shell runs there), glov pushes nothing,
  since the push would change files under it: it names the program, and the next run once it ends pushes. An attached
  session therefore does not get Mac commits made meanwhile. A shell idle at its prompt there does not hold the push.
- When the session ends or you detach, glov fetches the guest's branches into `refs/remotes/glov-<vm>/*` (no tags)
  and says how far the guest's branch is ahead, so `glov claude` waits for its ssh instead of replacing itself.
  Fetching runs no hooks and copies no git config, so nothing of the guest's runs on the Mac until you merge it.
  `glov vm fetch <vm>` fetches while the harness works, or after a `--herdr` session in herdr-app, which returns
  nothing to fetch after, and `glov vm stop` (without `--force`) fetches before it stops the VM, so what
  `glov vm delete` lists includes the agent's work after the session returned. The guest can never push to the Mac's
  repository.
- git reaches the guest over SSH as the host `glov-<vm>`, written to `ssh_config` in the glov home with the VM's key
  and `glov ssh-relay`, and with forwarding off as for `glov ssh`. glov's own git calls use that file. The first run in
  a terminal offers to add an `Include` of it at the top of `~/.ssh/config`, so `git fetch glov-<vm>` also works by
  hand; glov remembers a no (`ssh_config.declined` next to it).
- `glov vm land <vm> [<branch>]` brings the guest's new commits on the checked-out branch in as yours. It fetches,
  lists them with their files for you to review (`git log -p`), and once you agree (`--yes` without a terminal)
  makes each again on the branch with `git cherry-pick`. Each keeps its author, you are the committer, and it is
  signed as your git config signs (`commit.gpgsign`), with your key, which never enters the guest. Then it gives the
  guest the branch back (`--force-with-lease` against what it fetched), so the agent can push it and open a pull
  request with your signatures intact. The agent must not amend or rebase landed commits; new ones need another land.
  - A commit that does not apply stops the cherry-pick for you: resolve it and `git cherry-pick --continue` (or
    `--abort`), then run `glov vm land` again, which only gives the branch back.
  - If the guest committed in the meantime it keeps its branch; the next land takes only what it added.
  - A branch with merge commits from the guest is landed by hand.
- `glov vm delete` of a project's VM first lists the guest's commits that no local branch has, by content, and asks
  (`--yes` without a terminal), then removes the `glov-<vm>` remote, its land state and its SSH host too. It lists
  only what was fetched, and a VM that macOS or the guest shut down was not fetched by `glov vm stop`, so in a
  terminal it first offers to start the VM, fetch and stop it again (Enter does; a start or fetch that fails keeps the
  VM). With `--yes`, or without a terminal, it only warns that later commits are not listed.
- What you merge or land is still the guest's work: review it before you run the project's build, tests or scripts on
  the Mac.
- LFS files reach the guest as pointers only, and the guest updates submodules itself, through its own egress.

`live` shares the project read-write at the same path in the guest as the share `project` instead. It is the only mode
for a folder outside git, and glov warns when you choose it. In a git worktree, whose `.git` is a file naming its
repository's, that repository's `.git` is shared at its own path as `project-git` too, so git works in the guest; the
guest can then write the whole repository's git data (every branch, and the hooks all its worktrees run).

**Live mode trades the template's protection for convenience.** The guest writes the working tree itself, `.git`
included, so a hostile harness can plant git hooks, change git config, or edit `.envrc`, build scripts and editor
settings that run on the Mac the next time you use them there. Review what changed (outside `.git` too) before you run
git or the project's tools on the Mac after a session; for unattended work, use git mode, or the template and a clone.

### On a Windows host

On the Windows edition `glov claude` (`codex`, `agy`, `glov agent <harness>`) reaches the project through git by
default, so nothing the guest writes lands in the working tree on Windows. A new VM asks for the mode in a terminal
(`git`, the default, or `live`); `--sync git|live` chooses it without asking. A VM keeps its mode: delete it to change
the mode. In git mode:

- The project must be a git repository. The VM's own repository is at the project's `/mnt/<drive>/<path>` on the
  guest's disk. It commits under your `user.name` and `user.email`, unsigned: your signing key stays on Windows. Its
  `origin` is the project's `origin`, and the VM may reach that repository once you confirm, as on the Mac.
- Each run fetches the guest's branches, then pushes the current branch if the guest lacks commits of it, and checks
  the guest out on it. Uncommitted changes on Windows stay there, and glov says how many. A guest that is ahead keeps its
  commits; one whose branch went another way, or with changes not committed, keeps its state, and glov warns. Nothing
  is forced. While a program works in the guest's copy, glov pushes nothing and names it, as on the Mac; a shell idle at
  its prompt there does not hold the push.
- When the session ends or you detach, glov fetches the guest's branches into `refs/remotes/glov-<vm>/*` (no tags) and
  says how far the guest's branch is ahead. Fetching runs no hooks and copies no git config, so nothing of the guest's
  runs on Windows until you merge it. `glov vm fetch <vm>` fetches while the harness works, and `glov vm stop`
  (without `--force`) fetches before it stops the VM, so what `glov vm delete` lists includes the agent's work after
  you left. The guest can never push to the Windows repository.
- git reaches the guest over SSH as the host `glov-<vm>`, written to `%LOCALAPPDATA%\glov\ssh_config` with the VM's
  key and glov's relay. glov's own git calls use Windows' ssh with that file. The first run in a terminal offers to
  add an `Include` of it at the top of `~/.ssh/config`, so `git fetch glov-<vm>` also works by hand; glov remembers a
  no. The `Include` names the file from `~` (`~/AppData/Local/glov/ssh_config`) when it is in your profile: Git for
  Windows' own ssh reads `C:/...` there as a path inside `~/.ssh`.
- The VM is never frozen or reset (`glov vm harness install` installs without either): your work in it is on its
  disk. Its conversation records are in `%LOCALAPPDATA%\glov\sessions\<vm>\`, which `glov vm status` shows and `glov vm delete` keeps.
- `glov vm land <vm> [<branch>]` brings the guest's new commits on the checked-out branch in as yours. It fetches,
  lists them with their files for you to review (`git log -p`), and once you agree (`--yes` without a terminal)
  makes each again on the branch with `git cherry-pick`. Each keeps its author, you are the committer, and it is
  signed as your git config signs (`commit.gpgsign`), with your key, which never enters the guest. Then it gives the
  guest the branch back (`--force-with-lease` against what it fetched), so the agent can push it and open a pull
  request with your signatures intact. The agent must not amend or rebase landed commits; new ones need another land.
  - A commit that does not apply stops the cherry-pick for you: resolve it and `git cherry-pick --continue` (or
    `--abort`), then run `glov vm land` again, which only gives the branch back.
  - If the guest committed in the meantime it keeps its branch; the next land takes only what it added.
  - A branch with merge commits from the guest is landed by hand.
- `glov vm delete` of a project's VM first lists the guest's commits that no local branch has, by content, and asks
  (`--yes` without a terminal), then removes the `glov-<vm>` remote and its SSH host too. It lists only what was
  fetched, and a VM that Windows or the guest shut down was not fetched by `glov vm stop`, so in a terminal it first
  offers to start the VM, fetch and stop it again (Enter does; a start or fetch that fails keeps the VM). With
  `--yes`, or without a terminal, it only warns that later commits are not listed.
- What you merge is still the guest's work: review it before you run the project's build, tests or scripts on Windows.
- LFS files reach the guest as pointers only, and the guest updates submodules itself, through its own egress.

`live` shares the folder with the VM instead, as the live share `project` kept in step both ways (`.git` included),
at the project's `/mnt/<drive>/<path>`. It is the only mode for a folder outside git, and glov warns when you choose
it:

- What the guest writes takes effect on Windows at once: hooks, git config, `.envrc` and build scripts too. Review
  them before you use them.
- Do not run git on Windows while the agent works. While either side holds a `*.lock` file in `.git`, the rest of
  `.git` waits, and lock files are never copied. `glov share status <vm>` names a lock held for a minute: one that
  stays was left by a git that stopped, so delete it if no git runs there.
- A branch both sides moved keeps the newer side's, and the other side's as `<branch>-glov-conflict-<host|guest>` on
  both sides, which `glov share status <vm>` lists. Other files both sides changed keep the newer one.
- In a worktree the `.git` file stays out of the share; the repository it names is shared live too, as
  `project-git`, and the guest gets a `.git` naming that copy, with `gc.worktreePruneExpire=never` in its global git
  config. `core.autocrlf` and `core.untrackedCache` go to the guest's global config when Windows' git sets them true.
- `glov vm fetch` and `glov vm land` are for git mode only; commits made in the guest are already in the repository.

`--herdr` works in either mode as [on the Mac](#in-herdr), with the same risks, and these differences:

- hc (0.1.20 or later) reaches the VM through `"<glov.exe>" ssh <vm> --` as on the Mac, so `--herdr` needs no
  `Include` in `~/.ssh/config`.
- The harness starts in the project's guest path, `/mnt/<drive>/<path>`, which is also the folder of herdr-app's
  space for it.
- glov waits up to a minute for hc to reach the machine: after the VM starts, hc reports the last failure until it
  tries again.
- In git mode glov fetches the guest's branches when `hc attach` returns (you detach, or the harness ends). With
  herdr-app nothing returns: `glov vm fetch <vm>` brings them in, and `glov vm stop` does before it stops the VM.
- `glov vm delete` removes the machine with the VM's SSH host.

## Conversation records

Each harness's conversation records go to `<glov home>/.sessions/<vm>/<harness>/` on the Mac as they are written,
so they survive the revert that starts each task (`--no-sessions` at `vm create` turns this off). Read them to review what
a task did; never resume them with a harness on the Mac, since the guest wrote them.
[docs/networking.md](networking.md#harness-profiles) has the details.

## Subscriptions

```sh
glov secret login agent claude      # paste the token claude setup-token prints
glov secret login agent codex       # signs in in the VM, in this terminal; also antigravity
glov secret logout agent codex      # back to the API key
```

The harness then uses your subscription instead of an API key. Its tokens stay on the Mac; the proxy keeps the ones
each sign-in and refresh returns, and the guest holds placeholders.
[docs/networking.md](networking.md#subscriptions) has the details.

## Egress

```sh
glov egress list agent                     # the allowlist
glov egress log agent --denied --follow    # what the guest tried and was refused
glov egress allow agent pypi.org files.pythonhosted.org
glov egress remove agent pypi.org files.pythonhosted.org
glov secret grants agent                   # which secrets the VM may use
```

Changes apply to the next connection, without a restart. MCP clients get `egress_list` and `egress_log`, and
`egress_allow` and `egress_remove` only after `glov egress mcp agent on`. Secrets and grants have no MCP tool.
[docs/networking.md](networking.md#isolated-vz-vms) covers the proxy, the providers and the harness profiles.

### Open network

A harness that keeps hitting the allowlist (package registries, docs sites, tool downloads) can be given the public
internet instead, keeping the secret broker:

```sh
glov vm configure agent --egress open        # running or stopped; the next connection follows
glov vm configure agent --egress restricted  # back to the allowlist, which open mode kept
glov claude --egress open                     # a new project VM that starts open
```

The VM still has no network device: every connection goes through the egress proxy, which stops checking the allowlist
and nothing else.

| | Restricted | Open |
| --- | --- | --- |
| Outbound | allowlisted hosts only (443 and 80 unless an entry names a port) | any public host, any port |
| Private networks | `--private` entries only | `--private` entries only |
| Placeholder substitution, repository allowlists | bound hosts only | unchanged |
| Placeholders readable as real values in the guest | no | no |
| `tmpfs` secrets | reach allowlisted hosts only | can reach any host |
| Project data | leaves only to allowlisted hosts | can go anywhere |
| egress.log | every connection | every connection |
| Non-HTTP traffic, programs that ignore the proxy | no way out | no way out |

What open mode gives up is the limit on where data goes: a hostile harness can send your code, and any real value it
holds, to any host. Placeholder secrets stay safe, because the guest never holds their values and only the proxy
substitutes them, for their bound hosts. A VM whose harness takes a secret as a real value in tmpfs is opened, and such
a secret granted to an open VM, only with `--allow-plain-secrets`; with a terminal, glov asks instead. glovd keeps the
secrets so allowed in vm.json and, while the VM is open, keeps every other `tmpfs` secret out of the guest, so one a
harness added later (`glov vm harness add`) waits for `glov vm configure agent --egress open --allow-plain-secrets`.
Switching back to restricted forgets them. Only the CLI switches the mode: no MCP tool does, even after
`glov egress mcp agent on`.

## What it does not protect against

- **Leaks through allowed channels.** The allowlist limits where data can go, not whether it leaks; an open VM has no
  such limit (see [Open network](#open-network)). A hostile harness
  can put your code into its model-API requests, or push it to any allowlisted host, including any site that shares an
  allowed host's servers, since the proxy cannot see names inside TLS. Keep the run-time allowlist minimal, and bring
  results back with `git fetch` from the Mac instead of allowing writes to a code host.
- **Spending the key.** Hiding the key stops theft, not abuse: the guest can spend what the key allows, and glov caps
  neither requests nor tokens. Use a dedicated key, in a workspace or project with a provider-side hard spend limit.
- **Subscription logins.** glov substitutes API keys and other secrets only. Signing in to Claude Pro/Max, a ChatGPT plan or a Google
  account inside the VM puts that token in the guest, where the harness can read and use it.
- **Agents on the Mac.** An agent that can run `glov` in a Mac shell can change any VM's policy. So can an MCP client,
  once `glov egress mcp <vm> on` lets it.

Out of scope: Windows guests and non-HTTP egress such as `git push` over SSH.
