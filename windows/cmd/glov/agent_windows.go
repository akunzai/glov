package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/gitsignin"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/project"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/service"
	"github.com/akunzai/glov/windows/internal/share"
	"golang.org/x/sys/windows"
)

// agentReady is how long glov claude waits for a VM's Guest Service; the
// first boot takes longest.
const agentReady = 5 * time.Minute

// runAgent is glov claude (codex, agy): the harness on the current project,
// in an isolated VM of the project's own, reached through git or, chosen
// when the VM is made, a live share of the folder.
func runAgent(ctx context.Context, c *rpc.Client, cmd command) error {
	prof, err := harness.Named(cmd.agent.harness)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if cwd, err = share.Canonical(cwd); err != nil {
		return err
	}
	root, isGit := project.Root(cwd)
	if !isGit && cmd.agent.sync == api.SyncGit {
		return fmt.Errorf("%s is in no git repository: git init and commit first, or --sync live", cwd)
	}
	repo := hostRepo{dir: root}
	vm := project.VMName(root)
	fmt.Fprintf(os.Stderr, "%s: VM %s\n", root, vm)

	// By exact name: vm.status would take a VM whose name starts with it.
	var vms []api.VMInfo
	if err := call(ctx, c, api.MethodVMList, nil, &vms); err != nil {
		return err
	}
	var info api.VMInfo
	// refreshed holds the source secrets whose command ran in this start.
	refreshed := map[string]bool{}
	if i := slices.IndexFunc(vms, func(v api.VMInfo) bool { return v.Name == vm }); i < 0 {
		mode, err := chooseSync(cmd.agent.sync, isGit, root)
		if err != nil {
			return err
		}
		if info, err = createProjectVM(ctx, c, cmd, vm, root, repo, prof, mode, isGit, refreshed); err != nil {
			return err
		}
	} else if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil {
		return err
	} else if info.Project == nil || !strings.EqualFold(info.Project.Path, root) {
		return fmt.Errorf("VM %s is not the VM of %s; delete or rename it to give the project its own", vm, root)
	} else if cmd.agent.sync != "" && cmd.agent.sync != info.Project.Sync {
		return fmt.Errorf("VM %s reaches %s through %s: a VM keeps its mode, so glov vm delete %s first to make it again with --sync %s", vm, root, info.Project.Sync, vm, cmd.agent.sync)
	} else if !isGit && info.Project.Sync == api.SyncGit {
		return fmt.Errorf("%s is in no git repository any more: VM %s reaches it through git", root, vm)
	} else if cmd.egress != "" {
		var e api.Egress
		if err := call(ctx, c, api.MethodEgressList, api.EgressParams{VM: vm}, &e); err != nil {
			return err
		}
		if e.Open != (cmd.egress == "open") {
			return fmt.Errorf("VM %s already exists: change its egress with glov vm configure %s --egress %s", vm, vm, cmd.egress)
		}
	}

	if info.State != "running" {
		fmt.Fprintf(os.Stderr, "starting %s\n", vm)
		if err := call(ctx, c, api.MethodVMStart, api.VMParams{VM: vm}, &info); err != nil {
			return err
		}
	}
	if g := info.GuestService; g == nil || g.State != "ready" {
		fmt.Fprintf(os.Stderr, "waiting for %s's Guest Service (up to %d minutes; the first boot takes longest)\n", vm, int(agentReady.Minutes()))
		if err := call(ctx, c, api.MethodVMWait, api.WaitParams{VM: vm, State: api.WaitGuestReady, Timeout: agentReady.Seconds()}, &info); err != nil {
			return err
		}
	}
	ensureShareAgents(ctx, c, vm)
	if !slices.Contains(info.Harnesses, prof.Name) {
		// A project's VM takes each harness the first time it is asked for.
		if err := runHarnessAdd(ctx, c, vm, prof.Name, false); err != nil {
			return err
		}
		if err := grantStoredSecrets(ctx, c, vm, prof, cmd.allowPlain, refreshed); err != nil {
			return err
		}
	} else if code, _, err := guestRun(ctx, c, vm, []string{"test", "-x", "/usr/local/bin/" + prof.Command}); err != nil {
		return err
	} else if code != 0 {
		if err := runHarnessInstall(ctx, c, vm, nil, false); err != nil {
			return err
		}
	}
	refreshGranted(ctx, c, vm, refreshed)
	fmt.Fprintln(os.Stderr, "waiting for the console sign-in")
	if err := waitForUser(ctx, c, vm); err != nil {
		return err
	}

	guest, err := share.GuestPath(root)
	if err != nil {
		return err
	}
	live := info.Project.Sync == api.SyncLive
	branch := ""
	if live {
		if err := waitInStep(ctx, c, vm); err != nil {
			return err
		}
		if err := prepareLiveGuest(ctx, c, vm, root, guest, repo); err != nil {
			return err
		}
		if isGit {
			fmt.Fprintf(os.Stderr, "%s is live in %s: do not run git here while the agent works there\n", root, vm)
		}
	} else {
		if err := repo.connect(ctx, c, vm, guest); err != nil {
			return err
		}
		branch = repo.push(ctx, c, vm, guest)
	}

	if cmd.agent.herdr {
		attached, err := runHerdr(ctx, cmd, vm, root, guest, prof)
		if attached && !live {
			// Whatever the session's end, bring the guest's commits back.
			repo.fetch(vm, branch)
		}
		if _, ok := errors.AsType[exitCode](err); ok {
			return nil
		}
		return err
	}

	var out bytes.Buffer
	if code, err := runAsUser(ctx, c, vm, []string{"sh", "-c", "command -v tmux >/dev/null && { tmux -L \"$1\" list-sessions -F '#{session_name}' 2>/dev/null || true; }", "sh", prof.Name}, &out); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("%s has no tmux; glov %s runs the harness in a tmux session", vm, prof.Command)
	}
	existing := strings.Fields(out.String())
	session := project.Session(prof.Name, existing, cmd.agent.fresh)
	if slices.Contains(existing, session) {
		if len(cmd.agent.args) > 0 {
			fmt.Fprintf(os.Stderr, "attaching the running session %s; its harness keeps the arguments it started with (--new starts another)\n", session)
		}
		fmt.Fprintf(os.Stderr, "attaching %s\n", session)
	} else {
		fmt.Fprintf(os.Stderr, "starting %s (detach with Ctrl-b d)\n", session)
	}
	tmux := project.TmuxCommand(prof.Name, session, guest, append([]string{prof.Command}, cmd.agent.args...))
	sshErr := runSSH(ctx, c, vm, []string{"-t", tmux})
	if !live {
		// Whatever the session's end, bring the guest's commits back.
		repo.fetch(vm, branch)
	}
	if _, ok := errors.AsType[exitCode](sshErr); ok {
		return nil
	}
	return sshErr
}

// runProjectFetch is glov vm fetch: the guest's branches of a project's VM
// into its project.
func runProjectFetch(ctx context.Context, c caller, vm string) error {
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil {
		return err
	}
	if info.Project == nil || info.Project.Sync != api.SyncGit {
		return fmt.Errorf("VM %s is not a project's VM reached through git (glov claude)", info.Name)
	}
	if info.State != "running" {
		return fmt.Errorf("VM %s is %s: glov vm start %s", info.Name, info.State, info.Name)
	}
	if err := projectFetch(ctx, c, info); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "fetched %s's branches into %s as refs/remotes/%s/*\n", info.Name, info.Project.Path, project.Remote(info.Name))
	return nil
}

// fetchBeforeStop fetches the guest's branches of a running project VM
// reached through git before glov vm stop stops it: glov vm delete, which
// needs the VM stopped, can then list the guest's commits no local branch
// has, even those made after the session that started them returned (a
// herdr-app space never returns). A failure only warns: the VM still
// stops.
func fetchBeforeStop(ctx context.Context, c caller, vm string) {
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil ||
		info.Project == nil || info.Project.Sync != api.SyncGit || info.State != "running" {
		return
	}
	err := fmt.Errorf("%s's Guest Service is not ready, so its branches were not fetched", info.Name)
	if g := info.GuestService; g != nil && g.State == "ready" {
		err = projectFetch(ctx, c, info)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; the guest's commits since the last fetch are not in %s: glov vm start %s, then glov vm fetch %s\n",
			err, info.Project.Path, info.Name, info.Name)
		return
	}
	fmt.Fprintf(os.Stderr, "fetched %s's branches into %s\n", info.Name, info.Project.Path)
}

// projectFetch fetches the guest's branches of running project VM info
// into its project as refs/remotes/glov-<vm>/*. It sets SSH up in the
// guest again first, as glov ssh does: a VM turned off right after it was
// set up may have lost the key.
func projectFetch(ctx context.Context, c caller, info api.VMInfo) error {
	repo := hostRepo{dir: info.Project.Path}
	if _, err := sshHost(ctx, c, info.Name); err != nil {
		return err
	}
	if err := repo.useSSH(info.Name); err != nil {
		return err
	}
	branch, _ := repo.git("symbolic-ref", "--short", "-q", "HEAD")
	if !repo.fetch(info.Name, branch) {
		return fmt.Errorf("fetching from %s failed", info.Name)
	}
	return nil
}

// createProjectVM creates the project's isolated VM with the harness (and
// a live project's shares), grants the harness's stored secrets and, once
// the owner agrees, lets it reach the repositories of the project's
// remotes.
func createProjectVM(ctx context.Context, c *rpc.Client, cmd command, vm, root string, repo hostRepo, prof harness.Profile, mode string, isGit bool, refreshed map[string]bool) (api.VMInfo, error) {
	p := api.CreateParams{VM: vm, Isolated: true, Harnesses: []string{prof.Name}, Open: cmd.egress == "open",
		Project: &api.Project{Path: root, Sync: mode}}
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMCreate, p, &info); err != nil {
		return info, err
	}
	if err := addSessionsShare(ctx, c, vm); err != nil {
		return info, err
	}
	if mode == api.SyncLive {
		if err := addProjectShares(ctx, c, vm, root); err != nil {
			return info, err
		}
	}
	if err := grantStoredSecrets(ctx, c, vm, prof, cmd.allowPlain, refreshed); err != nil {
		return info, err
	}
	if isGit {
		if err := allowProjectRepos(ctx, c, vm, root, repo, cmd.agent.allowRemotes); err != nil {
			return info, err
		}
	}
	err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info)
	return info, err
}

// grantStoredSecrets grants the profile's secrets the owner has stored,
// running the commands of the source ones again first, and says which are
// not stored.
func grantStoredSecrets(ctx context.Context, c caller, vm string, prof harness.Profile, allowPlain bool, refreshed map[string]bool) error {
	secrets, err := secretList(ctx, c)
	if err != nil {
		return err
	}
	var grant, missing []string
	for _, s := range prof.Secrets {
		if slices.ContainsFunc(secrets, func(d api.Secret) bool { return d.Name == s.Name }) {
			grant = append(grant, s.Name)
		} else {
			missing = append(missing, s.Name)
		}
	}
	// A source secret's value is as fresh as its command's last run.
	for _, s := range commandSourcesIn(secrets, grant) {
		if err := refresh(ctx, c, s); err != nil {
			return err
		}
		refreshed[s.Name] = true
	}
	if len(grant) > 0 {
		var r api.SecretGrants
		err := confirmingPlainSecrets(allowPlain, isConsole(os.Stdin), askPlainSecrets, func(allow bool) error {
			return c.Call(ctx, api.MethodSecretGrant, api.SecretGrantParams{VM: vm, Names: grant, AllowPlainSecrets: allow}, &r)
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "granted %s to %s\n", strings.Join(grant, ", "), vm)
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "not stored yet: %s; glov secret set <name> --host <host>, then glov secret grant %s <name> (or glov secret login %s %s for a subscription)\n",
			strings.Join(missing, ", "), vm, vm, prof.Name)
	}
	return nil
}

// refreshGranted runs again the command of every source secret vm holds
// that has not run in this start, so a token granted earlier is not stale.
// A command that fails is a warning: the old value stays and the harness
// starts.
func refreshGranted(ctx context.Context, c caller, vm string, refreshed map[string]bool) {
	var grants api.SecretGrants
	if err := call(ctx, c, api.MethodSecretGrants, api.SecretGrantParams{VM: vm}, &grants); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list %s's secrets to refresh: %v\n", vm, err)
		return
	}
	list, err := secretList(ctx, c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list the secrets to refresh: %v\n", err)
		return
	}
	for _, s := range sourcesToRefresh(list, grants.Names, refreshed) {
		if err := refresh(ctx, c, s); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v; %s keeps its old value\n", err, s.Name)
		}
		refreshed[s.Name] = true
	}
}

// allowProjectRepos lets the VM reach the repositories the project's
// remotes name, grants the git secrets of their hosts and allows those
// hosts, once the owner confirms (project.Remotes).
func allowProjectRepos(ctx context.Context, c caller, vm, root string, repo hostRepo, allowFlag bool) error {
	var repos []egress.RepoPattern
	for _, url := range repo.remoteURLs() {
		if p, ok := project.RepoOf(url); ok && !slices.Contains(repos, p) {
			repos = append(repos, p)
		}
	}
	if len(repos) == 0 {
		return nil
	}
	dir, err := glovUserDir()
	if err != nil {
		return err
	}
	store := project.Remotes{Path: filepath.Join(dir, "project-remotes.json")}
	known, err := store.Confirmed(root)
	if err != nil {
		return err
	}
	secrets, err := secretList(ctx, c)
	if err != nil {
		return err
	}
	var git, allow, texts []string
	anyNew := false
	for _, r := range repos {
		texts = append(texts, r.String())
		anyNew = anyNew || !slices.Contains(known, r.String())
	}
	for _, s := range secrets {
		if s.Forge == "" {
			continue
		}
		e := gitsignin.Entry{Name: s.Name, Forge: egress.Forge(s.Forge), Hosts: s.Hosts}
		if slices.ContainsFunc(e.GitHosts(), func(h string) bool {
			return slices.ContainsFunc(repos, func(r egress.RepoPattern) bool { return r.Host == h })
		}) {
			git = append(git, s.Name)
			for _, h := range s.Hosts {
				if !slices.Contains(allow, h) {
					allow = append(allow, h)
				}
			}
		}
	}
	note := ""
	if anyNew {
		note = " (new: not confirmed for this project before)"
	}
	fmt.Fprintf(os.Stderr, "the project's git remotes name%s:\n", note)
	for _, t := range texts {
		mark := ""
		if !slices.Contains(known, t) {
			mark = "new: "
		}
		fmt.Fprintf(os.Stderr, "  %s%s\n", mark, t)
	}
	if len(git) > 0 {
		fmt.Fprintf(os.Stderr, "  git sign-in %s, allowing %s\n", strings.Join(git, ", "), strings.Join(allow, ", "))
	}
	terminal := isConsole(os.Stdin)
	choice := "Y/n"
	if anyNew {
		choice = "y/N"
	}
	if !project.ApplyRemotes(anyNew, terminal, allowFlag, func() string { return ask(fmt.Sprintf("let %s reach them? [%s] ", vm, choice)) }) {
		if terminal {
			fmt.Fprintf(os.Stderr, "not applied; glov secret repos %s add <repo> and glov secret grant %s <secret> do it by hand\n", vm, vm)
		} else {
			fmt.Fprintf(os.Stderr, "not applied without a terminal; run with --allow-remotes, or glov secret repos %s add <repo> by hand\n", vm)
		}
		return nil
	}
	if err := call(ctx, c, api.MethodSecretRepos, api.SecretReposParams{VM: vm, Add: texts}, nil); err != nil {
		return err
	}
	if len(git) > 0 {
		if err := call(ctx, c, api.MethodSecretGrant, api.SecretGrantParams{VM: vm, Names: git}, nil); err != nil {
			return err
		}
		if err := call(ctx, c, api.MethodEgressAllow, api.EgressParams{VM: vm, Hosts: allow}, nil); err != nil {
			return err
		}
	}
	if err := store.Confirm(root, texts); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s may reach %s (glov secret repos %s to change)\n", vm, strings.Join(texts, ", "), vm)
	return nil
}

// ask prints question on stderr and reads a line.
func ask(question string) string {
	fmt.Fprint(os.Stderr, question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return line
}

// glovUserDir is %LOCALAPPDATA%\glov, made if missing.
func glovUserDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "glov")
	return dir, os.MkdirAll(dir, 0o700)
}

// hostRepo is the project's repository on Windows. glov's own git calls
// reach the guest with Windows' ssh and glov's ssh_config, whatever the
// user's ssh setup.
type hostRepo struct {
	dir string
	// sshCommand is GIT_SSH_COMMAND, set by connect.
	sshCommand string
}

// git runs git in the repository and returns its trimmed output.
func (r hostRepo) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	if r.sshCommand != "" {
		cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+r.sshCommand)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git not found: install Git for Windows")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// remoteURLs are the URLs of the repository's remotes other than glov's.
func (r hostRepo) remoteURLs() []string {
	out, _ := r.git("config", "--get-regexp", `^remote\..*\.url$`)
	var urls []string
	for _, line := range strings.Split(out, "\n") {
		key, url, ok := strings.Cut(line, " ")
		if ok && !strings.HasPrefix(key, "remote.glov-") {
			urls = append(urls, url)
		}
	}
	return urls
}

// guestRepoScript makes $1 the guest's repository of the project as the
// guest user: a new repository takes pushes to its checked-out branch
// ($2) into the working tree. It commits as the owner ($3, $4), unsigned:
// the owner's key stays on Windows. $5 is the host's origin, if any.
const guestRepoScript = `set -e
cd "$1"
if [ ! -e .git ]; then
	git init -q
	git config receive.denyCurrentBranch updateInstead
	git symbolic-ref HEAD "refs/heads/$2"
fi
git config user.name "$3"
git config user.email "$4"
git config commit.gpgsign false
git config tag.gpgsign false
if [ -n "$5" ]; then
	git remote set-url origin "$5" 2>/dev/null || git remote add origin "$5"
fi
`

// connect sets up SSH to the guest (glov's ssh_config, and an Include in
// ~/.ssh/config once the owner agrees), the guest's repository at guest,
// and the repository's remote glov-<vm>.
func (r *hostRepo) connect(ctx context.Context, c caller, vm, guest string) error {
	path, err := sshHost(ctx, c, vm)
	if err != nil {
		return err
	}
	includeSSHConfig(path)
	if err := r.useSSH(vm); err != nil {
		return err
	}

	if code, out, err := guestRun(ctx, c, vm, []string{"install", "-d", "-o", sshUser, "-g", sshUser, guest}); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("making %s in the guest exited %d: %s", guest, code, strings.TrimSpace(string(out)))
	}
	branch, _ := r.git("symbolic-ref", "--short", "-q", "HEAD")
	if branch == "" {
		branch = "main"
	}
	name, _ := r.git("config", "user.name")
	email, _ := r.git("config", "user.email")
	// A sign-in written into the URL stays here: the guest gets the forge's
	// placeholder through glov secret instead.
	origin, _ := r.git("config", "--get", "remote.origin.url")
	origin = project.WithoutCredentials(origin)
	var out bytes.Buffer
	if code, err := runAsUser(ctx, c, vm, []string{"/bin/sh", "-c", guestRepoScript, "sh", guest, branch, name, email, origin}, &out); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("setting up the guest's repository exited %d: %s", code, strings.TrimSpace(out.String()))
	}

	remote, url := project.Remote(vm), project.Host(vm)+":"+guest
	if got, err := r.git("remote", "get-url", remote); err != nil {
		if _, err := r.git("remote", "add", remote, url); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "added the remote %s (%s) to %s\n", remote, url, r.dir)
	} else if got != url {
		if _, err := r.git("remote", "set-url", remote, url); err != nil {
			return err
		}
	}
	// The guest's tags stay out of the project's own.
	_, err = r.git("config", "remote."+remote+".tagOpt", "--no-tags")
	return err
}

// useSSH makes git reach the guest with Windows' ssh and glov's ssh_config,
// which must hold VM vm's host (connect writes it).
func (r *hostRepo) useSSH(vm string) error {
	dir, err := glovUserDir()
	if err != nil {
		return err
	}
	config := filepath.Join(dir, "ssh_config")
	if text, _ := os.ReadFile(config); !strings.Contains(string(text), "Host "+project.Host(vm)+"\n") {
		return fmt.Errorf("%s has no host %s: run glov claude (or codex, agy) in the project first", config, project.Host(vm))
	}
	ssh, err := openSSHTool("ssh")
	if err != nil {
		return err
	}
	r.sshCommand = `"` + filepath.ToSlash(ssh) + `" -F "` + filepath.ToSlash(config) + `"`
	return nil
}

// guestBranchScript checks the guest's repository ($1) out on branch $2,
// unless it is there already.
const guestBranchScript = `cd "$1" || exit 1
[ "$(git symbolic-ref --short -q HEAD)" = "$2" ] && exit 0
git checkout -q "$2"
`

// push sends the current branch to the guest and checks it out there, and
// returns the branch ("" for a detached HEAD). It fetches first and pushes
// only what the guest's branch lacks: a guest ahead keeps its commits, and
// one whose branch went another way, or with changes not committed, keeps
// its state and glov says so. It never forces.
func (r hostRepo) push(ctx context.Context, c caller, vm, guest string) string {
	branch, _ := r.git("symbolic-ref", "--short", "-q", "HEAD")
	if branch == "" {
		fmt.Fprintln(os.Stderr, "warning: HEAD is detached on Windows; nothing pushed to the guest")
		return ""
	}
	// A push moves the guest's working tree (updateInstead), and a checkout
	// switches it: not under a harness at work.
	var busy bytes.Buffer
	if code, err := runAsUser(ctx, c, vm, []string{"/bin/sh", "-c", project.GuestBusyScript, "sh", guest}, &busy); err == nil && code == 0 {
		program := strings.TrimSpace(busy.String())
		if program == "" {
			program = "a program"
		}
		fmt.Fprintf(os.Stderr, "not pushed: %s runs in %s in the guest, whose tree stays as it is; end it (exit the harness, or detach and close what runs there), then run again to push %s\n", program, guest, branch)
		return branch
	}
	if status, err := r.git("status", "--porcelain"); err == nil && status != "" {
		fmt.Fprintf(os.Stderr, "warning: %d changes not committed stay on Windows; the guest gets only commits\n", len(strings.Split(status, "\n")))
	}
	remote := project.Remote(vm)
	if _, err := r.git("fetch", "-q", remote); err != nil {
		fmt.Fprintf(os.Stderr, "warning: fetching from the guest failed: %v\n", err)
	}
	ref, theirs := "refs/heads/"+branch, "refs/remotes/"+remote+"/"+branch
	if _, err := r.git("rev-parse", "--verify", "-q", theirs); err == nil {
		if _, err := r.git("merge-base", "--is-ancestor", ref, theirs); err == nil {
			// The guest has all of it, and maybe more: nothing to send.
			return r.checkout(ctx, c, vm, guest, branch)
		}
		if _, err := r.git("merge-base", "--is-ancestor", theirs, ref); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s and %s/%s have diverged; the guest keeps its own (git log %s...%s/%s)\n", branch, remote, branch, branch, remote, branch)
			return r.checkout(ctx, c, vm, guest, branch)
		}
	}
	if _, err := r.git("push", "-q", remote, ref+":"+ref); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s was not pushed to the guest, which keeps its own state: %v\n", branch, err)
		return branch
	}
	fmt.Fprintf(os.Stderr, "pushed %s to the guest\n", branch)
	return r.checkout(ctx, c, vm, guest, branch)
}

// checkout puts the guest's repository on branch, unless changes not
// committed there keep it where it is.
func (r hostRepo) checkout(ctx context.Context, c caller, vm, guest, branch string) string {
	var out bytes.Buffer
	if code, err := runAsUser(ctx, c, vm, []string{"/bin/sh", "-c", guestBranchScript, "sh", guest, branch}, &out); err != nil || code != 0 {
		fmt.Fprintf(os.Stderr, "warning: the guest stays on its own branch, not %s: %s\n", branch, strings.TrimSpace(out.String()))
	}
	return branch
}

// fetch brings the guest's branches back as refs/remotes/glov-<vm>/* and
// says how far the guest's branch is ahead of the local one. It runs no
// hooks and copies no config: nothing of the guest's runs on Windows.
func (r hostRepo) fetch(vm, branch string) bool {
	remote := project.Remote(vm)
	if _, err := r.git("fetch", "-q", remote); err != nil {
		fmt.Fprintf(os.Stderr, "warning: fetching from the guest failed: %v\n", err)
		return false
	}
	if branch == "" {
		return true
	}
	if n, err := r.git("rev-list", "--count", branch+".."+remote+"/"+branch); err == nil && n != "0" {
		commits := "commits"
		if n == "1" {
			commits = "commit"
		}
		fmt.Fprintf(os.Stderr, "%s/%s is %s %s ahead of %s; review with git log %s..%s/%s\n", remote, branch, n, commits, branch, branch, remote, branch)
	}
	return true
}

// writeSSHConfig puts VM vm's Host block in %LOCALAPPDATA%\glov\ssh_config,
// and returns that file's path.
func writeSSHConfig(vm string, files sshFiles) (string, error) {
	dir, err := glovUserDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ssh_config")
	// The installed glov, which upgrades keep in place, rather than
	// whichever copy runs now.
	glov := filepath.Join(service.BinDir(), "glov.exe")
	if _, err := os.Stat(glov); err != nil {
		if glov, err = os.Executable(); err != nil {
			return "", err
		}
	}
	host := project.SSHHost{VM: vm, Key: files.key(), KnownHosts: files.knownHosts(),
		Relay: sshRelayCommand(filepath.ToSlash(glov), vm)}
	return path, editSSHConfig(path, func(text string) string { return project.WithSSHHost(text, host) })
}

// editSSHConfig rewrites glov's ssh_config at path with change, holding
// ssh_config.lock: glov runs for two projects at once would otherwise each
// write back what they read, dropping the other's VM.
func editSSHConfig(path string, change func(string) string) error {
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // closing releases the lock
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		return fmt.Errorf("locking %s.lock: %w", path, err)
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if text := change(string(old)); text != string(old) {
		return os.WriteFile(path, []byte(text), 0o600)
	}
	return nil
}

// sshHost makes VM vm reachable over SSH as the host glov-<vm>: its key,
// sshUser in the guest, and its block in glov's ssh_config, whose path it
// returns.
func sshHost(ctx context.Context, c caller, vm string) (string, error) {
	files, err := sshFilesFor(vm)
	if err != nil {
		return "", err
	}
	pub, err := ensureKey(files, vm)
	if err != nil {
		return "", err
	}
	if code, out, err := guestRun(ctx, c, vm, []string{"/bin/sh", "-c", sshSetup, "sh", pub}); err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("setting up SSH in the guest exited %d: %s", code, strings.TrimSpace(string(out)))
	}
	return writeSSHConfig(vm, files)
}

// userSSHConfig is ~/.ssh/config as it stands, for the Include of glov's
// ssh_config.
type userSSHConfig struct {
	// path is glov's ssh_config; file is ~/.ssh/config, in home.
	path, file, home, text string
}

// readUserSSHConfig reads ~/.ssh/config, missing or not.
func readUserSSHConfig(path string) (userSSHConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return userSSHConfig{}, err
	}
	u := userSSHConfig{path: path, file: filepath.Join(home, ".ssh", "config"), home: home}
	b, err := os.ReadFile(u.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return u, err
	}
	u.text = string(b)
	return u, nil
}

func (u userSSHConfig) included() bool { return project.HasInclude(u.text, u.path, u.home) }
func (u userSSHConfig) line() string   { return project.IncludeLine(u.path, u.home) }

// add puts the Include first in ~/.ssh/config.
func (u userSSHConfig) add() error {
	if err := os.MkdirAll(filepath.Dir(u.file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(u.file, []byte(project.WithInclude(u.text, u.path, u.home)), 0o600)
}

// includeSSHConfig asks once (glov remembers a no in ssh_config.declined)
// to include glov's ssh_config from ~/.ssh/config.
func includeSSHConfig(path string) {
	u, err := readUserSSHConfig(path)
	if err != nil || u.included() || !isConsole(os.Stdin) {
		return
	}
	declined := path + ".declined"
	if _, err := os.Stat(declined); err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "git fetch %s works by hand once %s has the line\n  %s\n", project.Remote("<vm>"), u.file, u.line())
	if a := strings.ToLower(strings.TrimSpace(ask("add it at the top? [Y/n] "))); a != "" && a != "y" && a != "yes" {
		os.WriteFile(declined, nil, 0o600)
		fmt.Fprintln(os.Stderr, "not added; glov still pushes and fetches itself")
		return
	}
	if err := u.add(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
}

// removeSSHHost drops VM vm's block from glov's ssh_config.
func removeSSHHost(vm string) {
	dir, err := glovUserDir()
	if err != nil {
		return
	}
	editSSHConfig(filepath.Join(dir, "ssh_config"), func(text string) string { return project.WithoutSSHHost(text, vm) })
}
