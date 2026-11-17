// glov is glov's command-line client on Windows. It drives glovd over
// its control pipe; `glov host install` sets glovd up.
package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/share"
	"github.com/akunzai/glov/windows/internal/vmname"
)

// Exit codes match the macOS glov: 1 for a failed command, 64 (EX_USAGE) for
// a command line it cannot parse.
const (
	exitFailure = 1
	exitUsage   = 64
)

const usage = `Usage: glov <command>

Commands:
  host install-broker Install or upgrade glov-broker, the service that
                      starts VMs for every user's glovd (elevated shell,
                      once); it also removes the old LocalSystem glovd
  host uninstall-broker
                      Remove glov-broker once no VM runs (elevated shell)
  host install [--cache DIR]
                      Install or upgrade glovd for you, in your glov home
                      (%LOCALAPPDATA%\glov, or GLOV_HOME), started at
                      sign-in; no elevation. --cache moves the image cache
                      to another local drive, and an upgrade without it
                      keeps the previous one
  host uninstall      Remove your glovd; VMs and data stay
  vm create <vm> [--distro NAME] [--harness NAME]... [--no-sessions] [--egress open|restricted] [--cpus N] [--memory SIZE] [--disk-size SIZE]
                      Create an isolated Linux VM from the newest build of
                      ubuntu (26.04 LTS, the default) or debian (13)
                      (defaults: 2 vCPUs, 4G, 32G; the disk can be at most
                      64T): it has no network adapter and reaches only the
                      hosts glov egress allows, through the egress proxy
                      (--isolated is accepted and changes nothing);
                      --harness claude, codex or antigravity
                      (repeatable) sets up that coding
                      agent: its settings, the hosts it needs, a placeholder
                      for its API key (glov secret grant gives it the
                      secret), and the user glov signed in on tty1; --egress
                      open lets it reach any public host, still only through
                      the egress proxy; --no-sessions keeps no conversation
                      records on this host (%LOCALAPPDATA%\glov\sessions\<vm>)
  vm list             List your VMs and their state (alias: vm ls)
  vm status <vm>      Show one VM's state; <vm> is a name or unique prefix
  vm start <vm>       Start a VM; it keeps running after glov exits
  vm stop <vm> [--force]
                      Shut a VM down, waiting up to 60 s before turning it
                      off; --force turns it off at once. A project VM
                      reached through git (glov claude) has its branches
                      fetched first, unless --force
  vm reboot <vm> [--timeout SECONDS]
                      Restart a VM and wait until its Guest Service answers
                      (default 600 s)
  vm reset <vm>       Return a stopped, frozen VM to its template: a
                      new disk, and the egress settings frozen with it;
                      shares stay; glov vm start runs it
  vm configure <vm> [--cpus N] [--memory SIZE] [--disk-size SIZE]
                      Change a stopped VM (alias: vm config); the disk only
                      grows, and the guest grows / on its next boot
  vm configure <vm> --egress open|restricted [--allow-plain-secrets]
                      Isolated VM, running or not: open lets the guest
                      reach any public host, still only through the egress
                      proxy (secrets and the egress log unchanged; private
                      networks still need egress allow --private);
                      restricted goes back to the allowlist. Applies to the
                      next connection. Opening a VM whose harness holds a
                      secret as a real value in tmpfs needs
                      --allow-plain-secrets, or a yes on a console
  vm trust list <vm>  Print the CA certificates an isolated VM trusts
  vm trust add <vm> <ca.pem> [--name NAME]
                      Trust one PEM certificate, such as a private network's
                      CA, in the guest's system store and in the egress
                      proxy's checks of the VM's intercepted hosts; NAME
                      (A-Z a-z 0-9 . _ -) defaults to the file name without
                      its extension and replaces one of that name. A running
                      VM is updated at once, a stopped one when it starts
  vm trust remove <vm> <name>
                      Stop trusting a certificate (alias: rm)
  vm delete <vm> [--yes]
                      Delete a stopped VM and its disk (alias: vm rm); <vm>
                      must be the exact name, and it asks unless --yes or -y.
                      A project VM's commits that no local branch has are
                      listed first, once glov offers to start it to fetch
                      them (not with --yes), and its glov-<vm> remote goes
                      with it
  vm console <vm> [--follow]
                      Print the serial console of the VM's current or last
                      boot, from firmware to login, without escape
                      sequences; --follow (-f) keeps printing until Ctrl+C
  vm wait <vm> [running|guest-ready] [--timeout SECONDS]
                      Wait until a VM runs, or its Guest Service answers
                      (default: running, 60 s)
  exec <vm> [--cwd DIR] [--as system|user] -- <command> [args...]
                      Run a command in the guest, without a shell (write
                      /bin/sh -c "..." for one), stream its output and exit
                      with its exit code; --cwd must exist; --as user runs in
                      the signed-in user's session and exits 75 while nobody
                      is signed in; Ctrl+C kills the command
  cp <src> <dst>      Copy a file or directory between the host and a guest;
                      write the guest side as <vm>:<path>, such as
                      dev:/root/proj (a one-letter prefix is a drive letter);
                      a directory's contents land at <dst>; the host side
                      must lie in one of the VM's shares (a rw one when
                      copying from the guest) or in
                      %LOCALAPPDATA%\glov\scratch
  share add <vm> <name> <host-folder> [--ro|--live] [--exclude PATTERN]...
                      Share a host folder with a VM: the guest sees it at
                      /mnt/<drive>/<path>, such as /mnt/c/src/proj; --ro
                      keeps cp from the guest out of it; --live keeps the
                      folder and the guest copy in step both ways while the
                      VM runs (a change made on both sides keeps the newer
                      one), through a glov share agent in the background;
                      --exclude takes a gitignore pattern a sync leaves
                      alone (repeatable)
  share remove <vm> <name>
                      Remove a share (alias: share rm); files already in the
                      guest stay
  share list <vm>     List a VM's shares (alias: share ls)
  share sync <vm> <name>[/<subdir>] [--verify]
                      Mirror a share's host folder, or one directory of it,
                      into the guest: changed files are written, files the
                      host lacks are deleted, excluded paths are left alone;
                      --verify rehashes every file on both sides; Ctrl+C
                      cancels it, in the guest too; for a live share, asks
                      its agent for a full round, starting the agent if
                      none runs
  share status <vm> [<name>] [--watch]
                      Show the running syncs and the file the guest is on,
                      and the live shares with their latest conflicts;
                      --watch (-w) repeats every 2 s until none runs
  egress list <vm>    Print an isolated VM's egress allowlist (alias: ls);
                      everything else is denied
  egress allow <vm> <host>... [--private]
                      Allow hosts (alias: add): a name, *.suffix (names
                      below suffix) or an IP address, for ports 443 and 80,
                      or with :port for that port only; applies to the next
                      connection, without a restart. A host must resolve to
                      a public address; --private (exact names or addresses
                      only) also lets it resolve to RFC 1918, CGNAT and
                      unique local ones, for a VPN or self-hosted service;
                      glov egress list marks it (private)
  egress remove <vm> <entry>...
                      Remove entries from the allowlist (alias: rm)
  egress log <vm> [--denied] [--follow] [--lines N]
                      Print the VM's egress connections (UTC time, decision,
                      target, bytes in and out, reason), the last 100 or N;
                      --denied only those refused (deny) or unreachable
                      (fail); --follow (-f) keeps printing until Ctrl+C
  egress mcp <vm> on|off
                      Let MCP clients change the allowlist (on), or not
                      (off, the default); glov mcp has no tool for this
  vm harness install <vm> [--harness NAME]... [--no-host-config]
                      Install the VM's harnesses (or only the --harness
                      ones) as the guest user, with the hosts they install
                      from allowed for the install only; the allowlist ends
                      as it was, Ctrl+C included. Then it copies your
                      harness config from your user profile into the guest
                      (instructions, rules, skills, agents and commands;
                      never settings, hooks or credentials; one way, links
                      followed; --no-host-config copies none), links the
                      conversation records to the harness-sessions share,
                      and stops the VM and freezes it as a template, egress
                      settings included; a frozen VM is reset first and the
                      install merged into the template. A project's VM
                      (glov claude) keeps running and is not frozen
  vm harness add <vm> <harness> [--no-host-config]
                      Add a harness to a running isolated VM created with
                      --harness, and install it: its environment, home files
                      (one already there is kept), secret links and runtime
                      hosts, as vm create --harness sets them up. Its
                      secrets still need glov secret grant. Not on a frozen
                      VM
  secret set <name> --host <host>... [--forge github|gitlab]
                      Store a secret for isolated VMs, read from stdin
                      (typed as one * per character on a console, then
                      confirmed), never from the command line; encrypted
                      for this machine (DPAPI) in glovd's data directory.
                      It may be sent only to the --host entries (as glov
                      egress allow takes them; repeat for more). --forge
                      makes it a git sign-in: git, gh and glab in a VM
                      granted it sign in with it, and its requests reach
                      only the VM's repositories (glov secret repos)
  secret source <name> --host <host>... [--forge github|gitlab] -- <command> [args...]
                      Take a secret's value from a command's stdout, such
                      as gh auth token: glov runs it as you (stdin closed,
                      stderr discarded, at most 10 s) now, at glov secret
                      grant, at each glov claude|codex|agy start and at
                      glov secret refresh, and stores what it prints as
                      set does
  secret refresh <name>
                      Run a source secret's command again and store its
                      value
  secret list [--json]
                      Print each secret's name, source and hosts, never a
                      value (alias: ls)
  secret remove <name>
                      Delete a secret and every VM's grant of it (alias: rm)
  secret grant <vm> <name>... [--allow-plain-secrets]
                      Let an isolated VM use secrets, and print its grants.
                      The guest holds glov-secret:<name>; the egress proxy
                      puts the value in its place in request headers sent
                      to the secret's hosts, over TLS it intercepts with
                      the egress CA the VM trusts. Source secrets are
                      refreshed first. An open VM gets a secret its harness
                      holds as a real value in tmpfs only with
                      --allow-plain-secrets, or a yes on a console
  secret revoke <vm> <name>...
                      Stop an isolated VM using secrets, from its next
                      request, and print its grants
  secret grants <vm>  Print the secrets an isolated VM may use
  secret repos <vm> [add|remove|list] [<repo>...]
                      Change or print the repositories an isolated VM may
                      reach with its git secrets: on their hosts, git, REST
                      and GraphQL requests that sign in must name one of
                      them, else 403 and an egress.log line. Give
                      <host>/<owner>/<repo>, or <host>/<group>/* for every
                      repository under a group; requests naming no
                      repository may only read. With none, the VM reaches
                      none
  secret login <vm> <harness>
                      Sign a harness in an isolated VM in to your
                      subscription instead of an API key. The tokens stay
                      on this host as secrets <vm>.<harness>.<name>,
                      granted to the VM; the guest holds placeholders.
                      claude: run claude setup-token anywhere and paste the
                      token it prints. codex and antigravity: the sign-in
                      runs in the VM in this terminal, and the egress proxy
                      keeps the tokens of each sign-in and refresh. The
                      harness is switched to the subscription now if the
                      VM runs, else when it next starts
  secret logout <vm> <harness>
                      Sign a harness out of your subscription and back to
                      its API key: signs out in the guest first when the VM
                      runs, then removes the tokens and the VM's grants
  claude|codex|agy [--new] [--sync git|live] [--allow-remotes] [--egress open|restricted] [--allow-plain-secrets] [--herdr [--yes]] [harness arguments...]
                      Open the harness on the current project (its git
                      root) in an isolated VM of the project's own,
                      creating it on the first run with the harness, the
                      secrets of it you stored, and, once you confirm, the
                      repositories of the project's remotes
                      (--allow-remotes without a terminal). The project
                      reaches the VM through git: each run pushes the
                      current branch to the guest's own repository at
                      /mnt/<drive>/<path>, and fetches the guest's
                      branches back as refs/remotes/glov-<vm>/* when it
                      starts and when you leave; nothing in the guest runs
                      on Windows until you merge it. --sync live (or live
                      at the first run's question) shares the folder
                      instead, kept in step both ways, .git included,
                      which a folder outside git needs: what the guest
                      writes there, hooks and git config too, takes effect
                      on Windows at once, and git must not run here while
                      the agent works. A VM keeps its mode; delete it to
                      change it. The harness runs in
                      a tmux session: detach with Ctrl-b d, run the same
                      command to attach; --new starts another session.
                      --herdr runs it in herdr-core instead: in herdr-app,
                      in a space of the VM's machine; elsewhere attached
                      here with hc (detach with ctrl+\). It makes the VM a
                      herdr-core machine, which herdr-core trusts: glov
                      prints what that lets a hostile guest do, and asks
                      the first time (--yes without a terminal). hc
                      reaches it through glov ssh <vm> --.
                      Arguments glov does not take go to the harness
                      (after -- when one is also glov's). Also glov agent
                      <harness>
  vm fetch <vm>       Fetch a project VM's branches (glov claude) into its
                      project as refs/remotes/glov-<vm>/*, as glov claude
                      does when it starts and when you leave
  vm land <vm> [<branch>] [--yes]
                      Make a project VM's new commits on the checked-out
                      branch (the guest's branch of that name) again on it
                      as your commits, signed as you sign, once you
                      reviewed them (--yes without a terminal), and give
                      the guest the branch back, so the agent may push it
                      and open a pull request. On a conflict it stops for
                      you to finish the cherry-pick; run it again after
  ssh <vm> [-- <ssh options> <command> [args...]]
                      Sign in to the guest as glov (sudo without a
                      password) with Windows' ssh, or run
                      a command there;
                      the key and known_hosts live in
                      %LOCALAPPDATA%\glov\vms\<vm>
  vm guest check <vm> Compare the VM's Guest Service with glovd's by
                      SHA-256, without changing the VM
  vm guest update <vm> [--force] [--timeout SECONDS]
                      Install glovd's Guest Service on a running VM and
                      wait until it answers (default 120 s), without a
                      reboot; nothing changes when the SHA-256 matches; it
                      is refused while exec jobs or a share sync run, which
                      --force cuts off
  vm stats [<vm>] [--watch] [--json]
                      Show the host's memory and each running VM's CPU use
                      on the host (100% is one host core), memory held on
                      the host, disk file size, and the guest's own memory,
                      disk and load; --watch (-w) refreshes every 2 s
  guide [troubleshooting]
                      Print the agent skill built into glov, or one of its
                      references
  mcp                 Serve the VM tools to an MCP client
                      over stdio (talks to glovd)
`

// command is one parsed command line.
type command struct {
	// "help", "version", "service <host verb>" or "vm <verb>".
	name  string
	vm    string
	cache string
	// force is vm stop's --force; yes is vm delete's --yes.
	force, yes bool
	// state and timeout are vm wait's and vm reboot's (seconds, 0 for the
	// default).
	state   string
	timeout float64
	// exec is exec's; VM is set too.
	exec api.ExecParams
	// sshExtra is what glov ssh passes on to ssh.
	sshExtra []string
	// src and dst are cp's.
	src, dst cpPath
	// create holds vm create's options; VM is set too.
	create api.CreateParams
	// share is share add's (HostPath as given) and names the share of
	// share remove and share status.
	share share.Share
	// ref, verify and watch are share sync's and share status's.
	ref           string
	verify, watch bool
	// noStart is vm create's --no-start.
	noStart bool
	// noSessions is vm create's --no-sessions: with --harness, no
	// conversation records share.
	noSessions bool
	// harnesses and noHostConfig are vm harness install's --harness and
	// --no-host-config (add's too, with the one harness it adds).
	harnesses    []string
	noHostConfig bool
	// json is stats' --json; topic is guide's.
	json  bool
	topic string
	// hosts, denied, lines and on are glov egress's (watch is log's
	// --follow).
	hosts  []string
	denied bool
	lines  int
	on     bool
	// names are glov secret's secret names (one, except for grant and
	// revoke); argv is source's command.
	names []string
	argv  []string
	// private is egress allow's --private; egress is vm configure's
	// --egress (open or restricted); allowPlain is --allow-plain-secrets.
	private    bool
	egress     string
	allowPlain bool
	// file is vm trust add's PEM file; names[0] is its name.
	file string
	// forge is secret set's and source's --forge; repos and repoAction are
	// secret repos' repositories and add, remove or list.
	forge      string
	repos      []string
	repoAction string
	// agent is glov claude's (codex's, agy's); egress and allowPlain are
	// its --egress and --allow-plain-secrets for a new VM.
	agent agentOptions
}

type usageError string

func (e usageError) Error() string { return string(e) }

// regrouped maps the grouped spelling a user types to the top-level word parse dispatches on.
// The words on the right are not commands themselves: parse rejects them as typed.
func regrouped(args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	rest := args[1:]
	switch args[0] {
	case "service", "guest", "harness", "stats", "trust":
		return nil, usageError(fmt.Sprintf("unknown command %q; see glov help", args[0]))
	case "host":
		return append([]string{"service"}, rest...), nil
	case "vm":
		if len(rest) > 0 && (rest[0] == "guest" || rest[0] == "harness" || rest[0] == "stats" || rest[0] == "trust") {
			return rest, nil
		}
	}
	return args, nil
}

func parse(args []string) (command, error) {
	if len(args) == 0 {
		return command{}, usageError("no command")
	}
	args, err := regrouped(args)
	if err != nil {
		return command{}, err
	}
	switch args[0] {
	case "version", "-v", "--version":
		return command{name: "version"}, nil
	case "help", "-h", "--help":
		return command{name: "help"}, nil
	case "mcp":
		if len(args) > 1 {
			return command{}, usageError("usage: glov mcp")
		}
		return command{name: "mcp"}, nil
	case "service":
		if len(args) < 2 || !slices.Contains([]string{"install", "uninstall", "install-broker", "uninstall-broker"}, args[1]) {
			return command{}, usageError("usage: glov host install|uninstall|install-broker|uninstall-broker")
		}
		cmd := command{name: "service " + args[1]}
		if args[1] != "install" {
			if len(args) > 2 {
				return command{}, usageError("usage: glov host " + args[1])
			}
			return cmd, nil
		}
		pos, err := options(args[2:], map[string]func(string) error{
			"cache": func(v string) error { cmd.cache = v; return nil },
		})
		if err != nil || len(pos) > 0 {
			return command{}, usageError("usage: glov host install [--cache DIR]")
		}
		return cmd, nil
	case "vm":
		if len(args) < 2 {
			return command{}, usageError("usage: glov vm create|list|status")
		}
		switch args[1] {
		case "create", "configure", "config":
			verb := map[string]string{"create": "create", "configure": "configure", "config": "configure"}[args[1]]
			cmd := command{name: "vm " + verb}
			opts := sizeOptions(&cmd.create)
			opts["egress"] = func(v string) error {
				if v != "open" && v != "restricted" {
					return fmt.Errorf("--egress %s: want open or restricted", v)
				}
				cmd.egress = v
				return nil
			}
			synopsis := "<vm> [--cpus N] [--memory SIZE] [--disk-size SIZE] | <vm> --egress open|restricted [--allow-plain-secrets]"
			rest := args[2:]
			if verb == "configure" {
				rest, cmd.allowPlain = flag(rest, "allow-plain-secrets")
			}
			if verb == "create" {
				// glovd makes every VM isolated; --isolated stays accepted.
				rest, cmd.create.Isolated = flag(rest, "isolated")
				rest, cmd.noStart = flag(rest, "no-start")
				rest, cmd.noSessions = flag(rest, "no-sessions")
				var err error
				if rest, cmd.create.Harnesses, err = repeated(rest, "harness"); err != nil {
					return command{}, err
				}
				for _, h := range cmd.create.Harnesses {
					if _, err := harness.Named(h); err != nil {
						return command{}, usageError("--harness " + err.Error())
					}
				}
				opts["distro"] = func(v string) error {
					if !slices.Contains(api.Distros, v) {
						return fmt.Errorf("--distro %s: want one of %s", v, strings.Join(api.Distros, ", "))
					}
					cmd.create.Distro = v
					return nil
				}
				synopsis = "<vm> [--distro " + strings.Join(api.Distros, "|") + "] [--harness <name>]... [--egress open|restricted] [--no-start] [--no-sessions] [--cpus N] [--memory SIZE] [--disk-size SIZE]"
			}
			pos, err := options(rest, opts)
			if err != nil {
				return command{}, usageError(err.Error())
			}
			if len(pos) != 1 {
				return command{}, usageError("usage: glov vm " + verb + " " + synopsis)
			}
			sized := cmd.create.CPUs != 0 || cmd.create.MemoryMiB != 0 || cmd.create.DiskGiB != 0
			switch {
			case verb == "create":
				cmd.create.Open = cmd.egress == "open"
			case !sized && cmd.egress == "":
				return command{}, usageError("glov vm configure: pass at least one of --cpus, --memory, --disk-size or --egress")
			case sized && cmd.egress != "":
				return command{}, usageError("glov vm configure: pass --egress on its own: it applies to a running VM, the other options need it stopped")
			case cmd.allowPlain && cmd.egress != "open":
				return command{}, usageError("--allow-plain-secrets goes with --egress open")
			}
			cmd.vm, cmd.create.VM = pos[0], pos[0]
			return cmd, nil
		case "delete", "rm":
			rest, yes := flag(args[2:], "yes")
			rest, y := flag(rest, "y")
			if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
				return command{}, usageError("usage: glov vm delete <vm> [--yes]")
			}
			return command{name: "vm delete", vm: rest[0], yes: yes || y}, nil
		case "console":
			rest, follow := flag(args[2:], "follow")
			rest, f := flag(rest, "f")
			if len(rest) != 1 || rest[0] == "" || strings.HasPrefix(rest[0], "-") {
				return command{}, usageError("usage: glov vm console <vm> [--follow]")
			}
			return command{name: "vm console", vm: rest[0], watch: follow || f}, nil
		case "land":
			rest, yes := flag(args[2:], "yes")
			if len(rest) < 1 || len(rest) > 2 || rest[0] == "" || strings.HasPrefix(rest[0], "-") || (len(rest) == 2 && strings.HasPrefix(rest[1], "-")) {
				return command{}, usageError("usage: glov vm land <vm> [<branch>] [--yes]")
			}
			cmd := command{name: "vm land", vm: rest[0], yes: yes}
			if len(rest) == 2 {
				cmd.ref = rest[1]
			}
			return cmd, nil
		case "fetch":
			if len(args) != 3 || args[2] == "" || strings.HasPrefix(args[2], "-") {
				return command{}, usageError("usage: glov vm fetch <vm>")
			}
			return command{name: "vm fetch", vm: args[2]}, nil
		case "list", "ls":
			if len(args) == 2 {
				return command{name: "vm list"}, nil
			}
			return command{}, usageError("usage: glov vm list")
		case "status", "start", "reset":
			if len(args) == 3 && args[2] != "" && !strings.HasPrefix(args[2], "-") {
				return command{name: "vm " + args[1], vm: args[2]}, nil
			}
			return command{}, usageError("usage: glov vm " + args[1] + " <vm>")
		case "stop":
			rest, force := flag(args[2:], "force")
			if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
				return command{}, usageError("usage: glov vm stop <vm> [--force]")
			}
			return command{name: "vm stop", vm: rest[0], force: force}, nil
		case "reboot", "wait":
			cmd := command{name: "vm " + args[1]}
			pos, err := options(args[2:], map[string]func(string) error{
				"timeout": func(v string) error {
					t, err := strconv.ParseFloat(v, 64)
					if err != nil || t <= 0 {
						return fmt.Errorf("--timeout %s: want a positive number of seconds", v)
					}
					cmd.timeout = t
					return nil
				},
			})
			if err != nil {
				return command{}, usageError(err.Error())
			}
			if args[1] == "wait" && len(pos) == 2 && (pos[1] == api.WaitRunning || pos[1] == api.WaitGuestReady) {
				cmd.state, pos = pos[1], pos[:1]
			}
			if len(pos) != 1 {
				if args[1] == "wait" {
					return command{}, usageError("usage: glov vm wait <vm> [running|guest-ready] [--timeout SECONDS]")
				}
				return command{}, usageError("usage: glov vm reboot <vm> [--timeout SECONDS]")
			}
			cmd.vm = pos[0]
			return cmd, nil
		}
		return command{}, usageError(fmt.Sprintf("unknown vm command %q", args[1]))
	}
	if args[0] == "exec" {
		return parseExec(args[1:])
	}
	if args[0] == "guest" {
		return parseGuest(args[1:])
	}
	if args[0] == "trust" {
		return parseTrust(args[1:])
	}
	if args[0] == "harness" {
		return parseHarness(args[1:])
	}
	if args[0] == "stats" {
		cmd := command{name: "stats"}
		rest, watch := flag(args[1:], "watch")
		rest, w := flag(rest, "w")
		rest, cmd.json = flag(rest, "json")
		cmd.watch = watch || w
		if len(rest) > 1 || (len(rest) == 1 && (rest[0] == "" || strings.HasPrefix(rest[0], "-"))) {
			return command{}, usageError("usage: glov vm stats [<vm>] [--watch] [--json]")
		}
		if len(rest) == 1 {
			cmd.vm = rest[0]
		}
		return cmd, nil
	}
	if args[0] == "guide" {
		if len(args) > 2 || (len(args) == 2 && args[1] != "troubleshooting") {
			return command{}, usageError("usage: glov guide [troubleshooting]")
		}
		cmd := command{name: "guide"}
		if len(args) == 2 {
			cmd.topic = args[1]
		}
		return cmd, nil
	}
	if args[0] == "share" {
		return parseShare(args[1:])
	}
	if args[0] == "egress" {
		return parseEgress(args[1:])
	}
	if args[0] == "secret" {
		return parseSecret(args[1:])
	}
	if args[0] == "agent" {
		if len(args) < 2 {
			return command{}, usageError(agentUse)
		}
		return parseAgent(args[1], args[2:])
	}
	if slices.Contains(agentCommands(), args[0]) {
		return parseAgent(args[0], args[1:])
	}
	if args[0] == "ssh" || args[0] == "ssh-relay" {
		// ssh-relay is ssh's ProxyCommand, which glov ssh sets.
		use := "usage: glov ssh <vm> [-- <ssh options> <command> [args...]]"
		if args[0] == "ssh-relay" {
			use = "usage: glov ssh-relay <vm>"
		}
		if len(args) < 2 || args[1] == "" || strings.HasPrefix(args[1], "-") {
			return command{}, usageError(use)
		}
		cmd := command{name: args[0], vm: args[1]}
		switch {
		case len(args) == 2:
		case args[0] == "ssh" && args[2] == "--":
			cmd.sshExtra = args[3:]
		default:
			return command{}, usageError(use)
		}
		return cmd, nil
	}
	if args[0] == "cp" {
		if len(args) != 3 {
			return command{}, usageError("usage: glov cp <src> <dst>, with <vm>:<path> on one side")
		}
		src, dst := parseCpPath(args[1]), parseCpPath(args[2])
		if (src.vm == "") == (dst.vm == "") {
			return command{}, usageError("glov cp: write exactly one side as <vm>:<path>")
		}
		if src.path == "" || dst.path == "" {
			return command{}, usageError("glov cp: a path is empty")
		}
		return command{name: "cp", src: src, dst: dst}, nil
	}
	return command{}, usageError(fmt.Sprintf("unknown command %q", args[0]))
}

func parseGuest(args []string) (command, error) {
	const use = "usage: glov vm guest check <vm> | glov vm guest update <vm> [--force] [--timeout SECONDS]"
	if len(args) == 0 || (args[0] != "check" && args[0] != "update") {
		return command{}, usageError(use)
	}
	cmd := command{name: "guest " + args[0]}
	rest := args[1:]
	if args[0] == "update" {
		rest, cmd.force = flag(rest, "force")
		var err error
		rest, err = options(rest, map[string]func(string) error{
			"timeout": func(v string) error {
				t, err := strconv.ParseFloat(v, 64)
				if err != nil || t <= 0 || t > api.MaxGuestUpdateTimeout {
					return fmt.Errorf("--timeout %s: want seconds, more than 0 and at most %d", v, api.MaxGuestUpdateTimeout)
				}
				cmd.timeout = t
				return nil
			},
		})
		if err != nil {
			return command{}, usageError(err.Error())
		}
	}
	if len(rest) != 1 || rest[0] == "" || strings.HasPrefix(rest[0], "-") {
		return command{}, usageError(use)
	}
	cmd.vm = rest[0]
	return cmd, nil
}

func parseExec(args []string) (command, error) {
	const use = "usage: glov exec <vm> [--cwd DIR] [--as system|user] -- <command> [args...]"
	cmd := command{name: "exec"}
	i := slices.Index(args, "--")
	if i < 0 || i == len(args)-1 {
		return command{}, usageError(use)
	}
	cmd.exec.Argv = args[i+1:]
	pos, err := options(args[:i], map[string]func(string) error{
		"cwd": func(v string) error { cmd.exec.Cwd = v; return nil },
		"as": func(v string) error {
			if v != "system" && v != "user" {
				return fmt.Errorf("--as %s: want system or user", v)
			}
			cmd.exec.As = v
			return nil
		},
	})
	if err != nil {
		return command{}, usageError(err.Error())
	}
	if len(pos) != 1 {
		return command{}, usageError(use)
	}
	cmd.vm, cmd.exec.VM = pos[0], pos[0]
	return cmd, nil
}

func parseShare(args []string) (command, error) {
	const use = "usage: glov share add|remove|list|sync|status <vm> ..."
	if len(args) == 0 {
		return command{}, usageError(use)
	}
	verb := map[string]string{"add": "add", "remove": "remove", "rm": "remove", "list": "list", "ls": "list", "sync": "sync", "status": "status", "agent": "agent"}[args[0]]
	cmd := command{name: "share " + verb}
	var pos []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case verb == "add" && (a == "--ro" || a == "--live"):
			if cmd.share.Mode != "" {
				return command{}, usageError("--ro and --live do not go together: a live share is written both ways")
			}
			cmd.share.Mode = map[string]share.Mode{"--ro": share.RO, "--live": share.Live}[a]
		case verb == "sync" && a == "--verify":
			cmd.verify = true
		case verb == "status" && (a == "--watch" || a == "-w"):
			cmd.watch = true
		case verb == "add" && (a == "--exclude" || strings.HasPrefix(a, "--exclude=")):
			p, ok := strings.CutPrefix(a, "--exclude=")
			if !ok {
				if i+1 == len(args) {
					return command{}, usageError("--exclude needs a pattern")
				}
				i++
				p = args[i]
			}
			if p == "" {
				return command{}, usageError("--exclude needs a pattern")
			}
			cmd.share.Exclude = append(cmd.share.Exclude, p)
		case strings.HasPrefix(a, "-") && a != "-":
			return command{}, usageError("unknown option " + a)
		default:
			pos = append(pos, a)
		}
	}
	switch {
	case verb == "add" && len(pos) == 3:
		cmd.vm, cmd.share.Name, cmd.share.HostPath = pos[0], pos[1], pos[2]
	case (verb == "remove" || verb == "agent") && len(pos) == 2:
		cmd.vm, cmd.share.Name = pos[0], pos[1]
	case verb == "list" && len(pos) == 1:
		cmd.vm = pos[0]
	case verb == "sync" && len(pos) == 2:
		cmd.vm, cmd.ref = pos[0], pos[1]
	case verb == "status" && (len(pos) == 1 || len(pos) == 2):
		cmd.vm = pos[0]
		if len(pos) == 2 {
			cmd.share.Name = pos[1]
		}
	case verb == "add":
		return command{}, usageError("usage: glov share add <vm> <name> <host-folder> [--ro|--live] [--exclude PATTERN]...")
	case verb == "remove":
		return command{}, usageError("usage: glov share remove <vm> <name>")
	case verb == "agent":
		return command{}, usageError("usage: glov share agent <vm> <name>")
	case verb == "list":
		return command{}, usageError("usage: glov share list <vm>")
	case verb == "sync":
		return command{}, usageError("usage: glov share sync <vm> <name>[/<subdir>] [--verify]")
	case verb == "status":
		return command{}, usageError("usage: glov share status <vm> [<name>] [--watch]")
	default:
		return command{}, usageError(use)
	}
	return cmd, nil
}

func parseEgress(args []string) (command, error) {
	const use = "usage: glov egress list|allow|remove|log|mcp <vm> ..."
	if len(args) == 0 {
		return command{}, usageError(use)
	}
	verb := map[string]string{"list": "list", "ls": "list", "allow": "allow", "add": "allow", "remove": "remove", "rm": "remove", "log": "log", "mcp": "mcp"}[args[0]]
	cmd := command{name: "egress " + verb}
	rest := args[1:]
	set := map[string]func(string) error{}
	if verb == "allow" {
		rest, cmd.private = flag(rest, "private")
	}
	if verb == "log" {
		var follow, f bool
		rest, cmd.denied = flag(rest, "denied")
		rest, follow = flag(rest, "follow")
		rest, f = flag(rest, "f")
		cmd.watch = follow || f
		set["lines"] = func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > api.MaxEgressLogLimit {
				return fmt.Errorf("--lines %s: want 1 to %d", v, api.MaxEgressLogLimit)
			}
			cmd.lines = n
			return nil
		}
	}
	pos, err := options(rest, set)
	if err != nil {
		return command{}, usageError(err.Error())
	}
	if len(pos) > 0 {
		cmd.vm = pos[0]
	}
	switch {
	case cmd.vm == "":
	case (verb == "list" || verb == "log") && len(pos) == 1:
		return cmd, nil
	case (verb == "allow" || verb == "remove") && len(pos) > 1:
		cmd.hosts = pos[1:]
		return cmd, nil
	case verb == "mcp" && len(pos) == 2 && (pos[1] == "on" || pos[1] == "off"):
		cmd.on = pos[1] == "on"
		return cmd, nil
	}
	switch verb {
	case "list":
		return command{}, usageError("usage: glov egress list <vm>")
	case "allow":
		return command{}, usageError("usage: glov egress allow <vm> <host>... [--private]")
	case "remove":
		return command{}, usageError("usage: glov egress remove <vm> <entry>...")
	case "log":
		return command{}, usageError("usage: glov egress log <vm> [--denied] [--follow] [--lines N]")
	case "mcp":
		return command{}, usageError("usage: glov egress mcp <vm> on|off")
	}
	return command{}, usageError(use)
}

// repeated takes every --name VALUE and --name=VALUE out of args, which
// options does not allow to repeat.
func repeated(args []string, name string) (rest, values []string, err error) {
	for i := 0; i < len(args); i++ {
		n, value, eq := strings.Cut(args[i], "=")
		if n != "--"+name {
			rest = append(rest, args[i])
			continue
		}
		if !eq {
			if i+1 == len(args) {
				return nil, nil, usageError("--" + name + " needs a value")
			}
			i++
			value = args[i]
		}
		if value == "" {
			return nil, nil, usageError("--" + name + " needs a value")
		}
		values = append(values, value)
	}
	return rest, values, nil
}

func parseSecret(args []string) (command, error) {
	const use = "usage: glov secret set|source|refresh|list|remove|grant|revoke|grants|repos|login|logout ..."
	if len(args) == 0 {
		return command{}, usageError(use)
	}
	verb := map[string]string{"set": "set", "source": "source", "refresh": "refresh", "list": "list", "ls": "list", "remove": "remove", "rm": "remove",
		"grant": "grant", "revoke": "revoke", "grants": "grants", "repos": "repos", "login": "login", "logout": "logout"}[args[0]]
	cmd := command{name: "secret " + verb}
	rest := args[1:]
	if verb == "source" {
		if i := slices.Index(rest, "--"); i >= 0 {
			rest, cmd.argv = rest[:i], rest[i+1:]
		}
	}
	var err error
	if verb == "set" || verb == "source" {
		if rest, cmd.hosts, err = repeated(rest, "host"); err != nil {
			return command{}, err
		}
	}
	if verb == "list" {
		rest, cmd.json = flag(rest, "json")
	}
	if verb == "grant" {
		rest, cmd.allowPlain = flag(rest, "allow-plain-secrets")
	}
	var set map[string]func(string) error
	if verb == "set" || verb == "source" {
		set = map[string]func(string) error{"forge": func(v string) error { cmd.forge = v; return nil }}
	}
	pos, err := options(rest, set)
	if err != nil {
		return command{}, usageError(err.Error())
	}
	named := len(pos) > 0 && pos[0] != ""
	if verb == "repos" && named {
		cmd.vm, cmd.repoAction = pos[0], "list"
		if len(pos) > 1 {
			cmd.repoAction = map[string]string{"add": "add", "remove": "remove", "rm": "remove", "list": "list", "ls": "list"}[pos[1]]
		}
		if len(pos) > 2 {
			cmd.repos = pos[2:]
		}
		switch {
		case cmd.repoAction == "":
			return command{}, usageError("give add, remove or list, then the repositories")
		case cmd.repoAction == "list" && len(cmd.repos) > 0:
			return command{}, usageError("list takes no repositories")
		case cmd.repoAction != "list" && len(cmd.repos) == 0:
			return command{}, usageError("name at least one repository")
		}
		return cmd, nil
	}
	switch {
	case verb == "list" && len(pos) == 0:
		return cmd, nil
	case (verb == "set" || verb == "source") && named && len(pos) == 1 && len(cmd.hosts) > 0 && (verb == "set" || len(cmd.argv) > 0):
		cmd.names = pos
		return cmd, nil
	case (verb == "refresh" || verb == "remove") && named && len(pos) == 1:
		cmd.names = pos
		return cmd, nil
	case (verb == "grant" || verb == "revoke") && named && len(pos) > 1:
		cmd.vm, cmd.names = pos[0], pos[1:]
		return cmd, nil
	case verb == "grants" && named && len(pos) == 1:
		cmd.vm = pos[0]
		return cmd, nil
	case (verb == "login" || verb == "logout") && named && len(pos) == 2 && pos[1] != "":
		cmd.vm, cmd.names = pos[0], pos[1:]
		return cmd, nil
	}
	switch verb {
	case "set":
		return command{}, usageError("usage: glov secret set <name> --host <host>... [--forge github|gitlab] (the value is read from stdin, never from the command line)")
	case "source":
		return command{}, usageError("usage: glov secret source <name> --host <host>... [--forge github|gitlab] -- <command> [args...]")
	case "refresh":
		return command{}, usageError("usage: glov secret refresh <name>")
	case "list":
		return command{}, usageError("usage: glov secret list [--json]")
	case "remove":
		return command{}, usageError("usage: glov secret remove <name>")
	case "grant":
		return command{}, usageError("usage: glov secret grant <vm> <name>... [--allow-plain-secrets]")
	case "revoke":
		return command{}, usageError("usage: glov secret revoke <vm> <name>...")
	case "grants":
		return command{}, usageError("usage: glov secret grants <vm>")
	case "repos":
		return command{}, usageError("usage: glov secret repos <vm> [add|remove|list] [<host>/<owner>/<repo>|<host>/<group>/*...]")
	case "login":
		return command{}, usageError("usage: glov secret login <vm> <harness>")
	case "logout":
		return command{}, usageError("usage: glov secret logout <vm> <harness>")
	}
	return command{}, usageError(use)
}

// cpPath is one side of glov cp: a guest path when vm is set.
type cpPath struct{ vm, path string }

// parseCpPath reads <vm>:<path>. A one-letter prefix is a drive letter
// (C:\work), so a host path; VM names are lowercase and longer in practice.
func parseCpPath(s string) cpPath {
	vm, path, ok := strings.Cut(s, ":")
	if !ok || len(vm) < 2 || vmname.Valid(vm) != nil {
		return cpPath{path: s}
	}
	return cpPath{vm: vm, path: path}
}

// sizeOptions are vm create's and vm configure's options, which set p.
func sizeOptions(p *api.CreateParams) map[string]func(string) error {
	return map[string]func(string) error{
		"cpus": func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return fmt.Errorf("--cpus %s: want a positive number", v)
			}
			p.CPUs = n
			return nil
		},
		"memory": func(v string) error {
			mib, err := sizeMiB(v, 1)
			if err != nil {
				return fmt.Errorf("--memory %s: %v", v, err)
			}
			p.MemoryMiB = mib
			return nil
		},
		"disk-size": func(v string) error {
			gib, err := parseDiskGiB(v)
			if err != nil {
				return fmt.Errorf("--disk-size %s: %v", v, err)
			}
			p.DiskGiB = gib
			return nil
		},
	}
}

// flag removes every --name (or -name for a one-letter name) from args and reports whether there was one.
func flag(args []string, name string) ([]string, bool) {
	var rest []string
	found := false
	dashes := "--"
	if len(name) == 1 {
		dashes = "-"
	}
	for _, a := range args {
		if a == dashes+name {
			found = true
		} else {
			rest = append(rest, a)
		}
	}
	return rest, found
}

// options parses --name VALUE and --name=VALUE options with the given
// setters and returns the other arguments. Each option may appear once.
func options(args []string, set map[string]func(string) error) ([]string, error) {
	var pos []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		fn, ok := set[name]
		if !ok || !strings.HasPrefix(a, "--") {
			return nil, fmt.Errorf("unknown option %s", a)
		}
		if seen[name] {
			return nil, fmt.Errorf("--%s given twice", name)
		}
		seen[name] = true
		if !hasValue {
			if i+1 == len(args) {
				return nil, fmt.Errorf("--%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if value == "" {
			return nil, fmt.Errorf("--%s needs a value", name)
		}
		if err := fn(value); err != nil {
			return nil, err
		}
	}
	return pos, nil
}

// parseDiskGiB parses a disk size for glovd, which takes whole GiB: the
// CLI's --disk-size and MCP's diskSize alike. It needs a unit.
func parseDiskGiB(s string) (int, error) {
	mib, err := sizeMiB(s, 0)
	if err == nil && mib%1024 != 0 {
		err = fmt.Errorf("want whole gigabytes")
	}
	return mib / 1024, err
}

// sizeMiB parses a size such as 512M, 4G or 1T (binary units; a trailing B
// or iB is allowed) into MiB. A bare number is taken as bareUnit MiB, as
// QEMU's -m reads it; bareUnit 0 requires a unit.
func sizeMiB(s string, bareUnit int) (int, error) {
	t := strings.ToUpper(s)
	t = strings.TrimSuffix(strings.TrimSuffix(t, "B"), "I")
	unit := bareUnit
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'M':
			unit, t = 1, t[:n-1]
		case 'G':
			unit, t = 1024, t[:n-1]
		case 'T':
			unit, t = 1024*1024, t[:n-1]
		}
	}
	n, err := strconv.Atoi(t)
	switch {
	case unit == 0:
		return 0, fmt.Errorf("want a unit, such as 64G")
	case err != nil || n < 1:
		return 0, fmt.Errorf("want a size such as 4G or 4096M")
	case n > (1<<31-1)/unit:
		return 0, fmt.Errorf("too large")
	}
	return n * unit, nil
}

// printList prints vm list's table, as on macOS: name, engine, state.
func printList(w io.Writer, vms []vmRow, empty string) {
	if len(vms) == 0 {
		fmt.Fprintln(w, empty)
		return
	}
	for _, v := range vms {
		fmt.Fprintf(w, "%-24s%-6s%s\n", v.name, v.engine, v.state)
	}
}

type vmRow struct{ name, engine, state string }

// parseHarness parses vm harness install and vm harness add.
func parseHarness(args []string) (command, error) {
	const usage = "usage: glov vm harness install <vm> [--harness NAME]... [--no-host-config], or glov vm harness add <vm> <harness> [--no-host-config]"
	if len(args) == 0 || (args[0] != "install" && args[0] != "add") {
		return command{}, usageError(usage)
	}
	cmd := command{name: "harness " + args[0]}
	rest, noHostConfig := flag(args[1:], "no-host-config")
	cmd.noHostConfig = noHostConfig
	var err error
	if args[0] == "install" {
		if rest, cmd.harnesses, err = repeated(rest, "harness"); err != nil {
			return command{}, err
		}
	}
	want := 1
	if args[0] == "add" {
		want = 2
	}
	if len(rest) != want || slices.ContainsFunc(rest, func(a string) bool { return a == "" || strings.HasPrefix(a, "-") }) {
		return command{}, usageError(usage)
	}
	cmd.vm = rest[0]
	if args[0] == "add" {
		cmd.harnesses = []string{rest[1]}
	}
	return cmd, nil
}
