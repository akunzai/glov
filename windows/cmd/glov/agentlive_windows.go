package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/project"
	"github.com/akunzai/glov/windows/internal/share"
)

// liveInStepWait is how long glov claude waits for a live project's first
// round, which copies the whole folder the first time.
const liveInStepWait = 30 * time.Minute

// liveWarning is what choosing live means.
const liveWarning = "warning: live keeps %s in step with the VM both ways, .git included: what the guest writes there, " +
	"hooks, git config, .envrc and build scripts too, takes effect on Windows at once; review them before you use them. " +
	"Do not run git here while the agent works: the two sides' git would change one repository at once " +
	"(a branch both moved keeps Windows' or the guest's as <branch>-glov-conflict-<side>, which glov share status lists)\n"

// chooseSync is how a new project VM reaches the project: --sync, else
// asked in a terminal (git by default), else git. A folder outside git
// can only be shared live, which must be chosen.
func chooseSync(flag string, isGit bool, root string) (string, error) {
	terminal := isConsole(os.Stdin)
	if !isGit {
		switch {
		case flag == api.SyncLive:
		case flag == "" && terminal:
			if a := strings.ToLower(strings.TrimSpace(ask(fmt.Sprintf("%s is in no git repository, which glov reaches its VM through; share the folder live instead? [y/N] ", root)))); a != "y" && a != "yes" {
				return "", errors.New("not created; git init and commit first, or --sync live")
			}
		default:
			return "", fmt.Errorf("%s is in no git repository: glov reaches the VM through git (git init and commit first), or --sync live shares the folder", root)
		}
		fmt.Fprintf(os.Stderr, liveWarning, root)
		return api.SyncLive, nil
	}
	mode := flag
	if mode == "" && terminal {
		fmt.Fprintf(os.Stderr, "how should the VM reach %s?\n"+
			"  git:  your commits go to a repository on the VM's disk and its commits come back to review (the default)\n"+
			"  live: the folder itself, .git included, is kept in step both ways\n", root)
		switch a := strings.ToLower(strings.TrimSpace(ask("[git/live] "))); a {
		case "", "git":
			mode = api.SyncGit
		case "live":
			mode = api.SyncLive
		default:
			return "", fmt.Errorf("not created: %q is neither git nor live", a)
		}
	}
	if mode == api.SyncLive {
		fmt.Fprintf(os.Stderr, liveWarning, root)
		return mode, nil
	}
	return api.SyncGit, nil
}

// addProjectShares shares a live project's folder with its VM, and the
// repository a worktree's git data lives in. The worktree's .git names it
// by its Windows path, so the guest gets a .git of its own instead
// (prepareLiveGuest).
func addProjectShares(ctx context.Context, c caller, vm, root string) error {
	s := share.Share{Name: project.ShareName, HostPath: root, Mode: share.Live}
	wt, isWorktree := project.GitDirectory(root)
	if isWorktree {
		s.Exclude = []string{"/.git"}
	} else if fi, err := os.Stat(filepath.Join(root, ".git")); err == nil && fi.IsDir() {
		s.GitDirs = []string{".git"}
	}
	if err := call(ctx, c, api.MethodShareAdd, api.ShareAddParams{VM: vm, Share: s}, nil); err != nil {
		return fmt.Errorf("the project share: %w", err)
	}
	fmt.Fprintf(os.Stderr, "shared %s live with %s\n", root, vm)
	if !isWorktree {
		return nil
	}
	common, err := share.Canonical(wt.Common)
	if err != nil {
		return err
	}
	g := share.Share{Name: project.GitShareName, HostPath: common, Mode: share.Live, GitDirs: []string{""}}
	if err := call(ctx, c, api.MethodShareAdd, api.ShareAddParams{VM: vm, Share: g}, nil); err != nil {
		return fmt.Errorf("the share of the worktree's repository: %w", err)
	}
	fmt.Fprintf(os.Stderr, "shared its repository %s live too (a worktree's git data lives there)\n", common)
	return nil
}

// waitInStep waits for the first round of the VM's live project shares.
func waitInStep(ctx context.Context, c caller, vm string) error {
	var list []api.ShareInfo
	if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: vm}, &list); err != nil {
		return err
	}
	var names []string
	for _, s := range list {
		if s.Name == project.ShareName || s.Name == project.GitShareName {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("VM %s has no share %s: delete it and run again", vm, project.ShareName)
	}
	told := false
	deadline, noAgent := time.Now().Add(liveInStepWait), time.Now().Add(time.Minute)
	for {
		var running []api.SyncProgress
		if err := call(ctx, c, api.MethodShareStatus, api.ShareStatusParams{VM: vm}, &running); err != nil {
			return err
		}
		waiting := slices.DeleteFunc(slices.Clone(names), func(n string) bool {
			return slices.ContainsFunc(running, func(p api.SyncProgress) bool { return p.Share == n && p.InStep })
		})
		if len(waiting) == 0 {
			return nil
		}
		agents := slices.ContainsFunc(running, func(p api.SyncProgress) bool { return p.Stage == api.StageLive && slices.Contains(waiting, p.Share) })
		switch {
		case !agents && time.Now().After(noAgent):
			return fmt.Errorf("no agent keeps %s in step: glov share sync %s %s starts one (glov share status %s shows why)", strings.Join(waiting, ", "), vm, waiting[0], vm)
		case time.Now().After(deadline):
			return fmt.Errorf("%s not in step after %d minutes: glov share status %s", strings.Join(waiting, ", "), int(liveInStepWait.Minutes()), vm)
		case !told:
			fmt.Fprintf(os.Stderr, "waiting for %s to be in step (the first round copies the whole folder)\n", strings.Join(waiting, ", "))
			told = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// prepareLiveGuest sets the guest's git up for a live project at guest:
// a worktree's .git naming its repository's guest path, kept from pruning
// (the repository names the worktree by its Windows path), and
// core.autocrlf and core.untrackedCache as Windows' git has them, so both
// sides see one work tree and read the index Windows' git writes without a
// warning.
func prepareLiveGuest(ctx context.Context, c caller, vm, root, guest string, repo hostRepo) error {
	gitdir := ""
	if wt, ok := project.GitDirectory(root); ok {
		common, err := share.Canonical(wt.Common)
		if err != nil {
			return err
		}
		g, err := share.GuestPath(common)
		if err != nil {
			return err
		}
		gitdir = g + "/worktrees/" + filepath.Base(wt.Dir)
	}
	var set []string
	for _, k := range []string{"core.autocrlf", "core.untrackedCache"} {
		if v, _ := repo.git("config", "--type=bool", "--get", k); v == "true" {
			set = append(set, k)
		}
	}
	script := `set -e
if [ -n "$2" ]; then
	printf 'gitdir: %s\n' "$2" > "$1/.git"
	git config --global gc.worktreePruneExpire never
fi
shift 2
for k; do git config --global "$k" true; done`
	var out bytes.Buffer
	code, err := runAsUser(ctx, c, vm, append([]string{"sh", "-c", script, "sh", guest, gitdir}, set...), &out)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("setting the guest's git up for %s failed: %s", guest, strings.TrimSpace(out.String()))
	}
	return nil
}
