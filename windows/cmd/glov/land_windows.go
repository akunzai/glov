package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/project"
)

// landRef keeps, while glov vm land is under way, the guest's commit it
// lands: refs/glov-land/<vm>/<branch>. Fetches leave it alone, and the push
// back expects the guest's branch there still. landBase keeps the local
// branch as it was before: once the owner finished a stopped cherry-pick
// the branch is past it, and if they aborted it, it is there.
func landRef(vm, branch string) string  { return "refs/glov-land/" + vm + "/" + branch }
func landBase(vm, branch string) string { return "refs/glov-land/" + vm + "/" + branch + ".base" }

// landedRef is the guest's commit last landed whose push back the guest
// refused, having committed since: the next land takes only what follows
// it, since a commit whose conflict the owner resolved differs from the
// guest's.
func landedRef(vm, branch string) string { return "refs/glov-land/" + vm + "/" + branch + ".landed" }

// projectRepo is a running project VM's repository on Windows, with git
// set up to reach the guest.
func projectRepo(ctx context.Context, c caller, vm string) (api.VMInfo, hostRepo, error) {
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil {
		return info, hostRepo{}, err
	}
	if info.Project == nil || info.Project.Sync != api.SyncGit {
		return info, hostRepo{}, fmt.Errorf("VM %s is not a project's VM reached through git (glov claude)", info.Name)
	}
	if info.State != "running" {
		return info, hostRepo{}, fmt.Errorf("VM %s is %s: glov vm start %s", info.Name, info.State, info.Name)
	}
	repo := hostRepo{dir: info.Project.Path}
	files, err := sshFilesFor(info.Name)
	if err != nil {
		return info, repo, err
	}
	if _, err := writeSSHConfig(info.Name, files); err != nil {
		return info, repo, err
	}
	return info, repo, repo.useSSH(info.Name)
}

// runLand is glov vm land: the guest's new commits on branch (the current
// one by default) made again on the local branch, after the owner reviewed
// them, so they are the owner's commits, signed as the owner signs; then
// the branch goes back to the guest, which may push it and open a pull
// request with the signatures intact.
func runLand(ctx context.Context, c caller, vm, branch string, yes bool) error {
	info, repo, err := projectRepo(ctx, c, vm)
	if err != nil {
		return err
	}
	vm = info.Name
	current, _ := repo.git("symbolic-ref", "--short", "-q", "HEAD")
	if branch == "" {
		branch = current
	}
	if branch == "" || branch != current {
		return fmt.Errorf("check %s out in %s first: glov vm land makes the guest's commits again on the checked-out branch", or(branch, "a branch"), repo.dir)
	}
	if busy := repo.pickInProgress(); busy {
		return fmt.Errorf("a cherry-pick is under way in %s: resolve it and git cherry-pick --continue (or --abort), then glov vm land %s again", repo.dir, vm)
	}
	remote := project.Remote(vm)
	theirs, pending := "refs/remotes/"+remote+"/"+branch, landRef(vm, branch)

	// A land that stopped at a conflict, now finished: only the push back
	// is left.
	if sha, err := repo.git("rev-parse", "--verify", "-q", pending); err == nil {
		base, _ := repo.git("rev-parse", "--verify", "-q", landBase(vm, branch))
		tip, _ := repo.git("rev-parse", "HEAD")
		if _, err := repo.git("merge-base", "--is-ancestor", base, tip); err == nil && base != "" && tip != base {
			return repo.pushBack(vm, branch, sha)
		}
		repo.git("update-ref", "-d", pending)
		repo.git("update-ref", "-d", landBase(vm, branch))
	}

	if !repo.fetch(vm, "") {
		return fmt.Errorf("fetching from %s failed", vm)
	}
	sha, err := repo.git("rev-parse", "--verify", "-q", theirs)
	if err != nil {
		return fmt.Errorf("the guest has no branch %s", branch)
	}
	limit, _ := repo.git("rev-parse", "--verify", "-q", landedRef(vm, branch))
	if _, err := repo.git("merge-base", "--is-ancestor", or(limit, "-"), sha); limit != "" && err != nil {
		limit = "" // the guest's branch went another way since
	}
	exclude := "^" + branch
	if limit != "" {
		exclude = "^" + limit
	}
	if merges, _ := repo.git("rev-list", "--merges", sha, "^"+branch, exclude); merges != "" {
		return fmt.Errorf("%s/%s has merge commits that %s lacks: land them by hand (git log %s..%s)", remote, branch, branch, branch, theirs)
	}
	picks, err := repo.unlanded(branch, sha, limit)
	if err != nil {
		return err
	}
	if len(picks) == 0 {
		if _, err := repo.git("merge-base", "--is-ancestor", sha, branch); err == nil {
			fmt.Fprintf(os.Stderr, "%s/%s has nothing %s lacks\n", remote, branch, branch)
			return nil
		}
		// Landed before, under other hashes: give the guest the local ones.
		return repo.pushBack(vm, branch, sha)
	}
	if status, _ := repo.git("status", "--porcelain", "--untracked-files=no"); status != "" {
		return fmt.Errorf("%s has changes not committed: commit or stash them first", repo.dir)
	}
	log, _ := repo.git(append([]string{"log", "--no-walk=unsorted", "--format=%h %s (%an)"}, picks...)...)
	stat, _ := repo.git(append([]string{"show", "--format=", "--stat=100"}, picks...)...)
	fmt.Fprintf(os.Stderr, "%s/%s has %s for %s:\n%s\n%s\n", remote, branch, count(len(picks), "commit"), branch, indent(log), indent(lastLine(stat)))
	fmt.Fprintf(os.Stderr, "review them first: git log -p %s..%s\n", branch, theirs)
	if !yes {
		if !isConsole(os.Stdin) {
			return errors.New("landing them needs your yes: run in a terminal, or with --yes once you reviewed them")
		}
		if a := strings.ToLower(strings.TrimSpace(ask(fmt.Sprintf("make them again on %s as your commits? [y/N] ", branch)))); a != "y" && a != "yes" {
			return errors.New("not landed")
		}
	}
	tip, err := repo.git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if _, err := repo.git("update-ref", landBase(vm, branch), tip); err != nil {
		return err
	}
	if _, err := repo.git("update-ref", pending, sha); err != nil {
		return err
	}
	// cherry-pick keeps each author, makes the owner the committer and
	// signs as commit.gpgsign says; one already applied is dropped.
	if _, err := repo.git(append([]string{"cherry-pick", "--empty=drop"}, picks...)...); err != nil {
		if repo.pickInProgress() {
			return fmt.Errorf("%v\nthe cherry-pick stopped: fix that in %s and git cherry-pick --continue (or --abort), then glov vm land %s again to give the guest the branch", err, repo.dir, vm)
		}
		repo.git("update-ref", "-d", pending)
		repo.git("update-ref", "-d", landBase(vm, branch))
		return err
	}
	return repo.pushBack(vm, branch, sha)
}

// unlanded are the commits of theirs not on branch, by content, oldest
// first, after limit if set: a commit landed before under another hash is
// not one of them.
func (r hostRepo) unlanded(branch, theirs, limit string) ([]string, error) {
	args := []string{"cherry", branch, theirs}
	if limit != "" {
		args = append(args, limit)
	}
	out, err := r.git(args...)
	if err != nil {
		return nil, err
	}
	var picks []string
	for _, line := range strings.Split(out, "\n") {
		if sha, ok := strings.CutPrefix(line, "+ "); ok {
			picks = append(picks, sha)
		}
	}
	return picks, nil
}

// pickInProgress reports a cherry-pick stopped at a conflict.
func (r hostRepo) pickInProgress() bool {
	_, err := r.git("rev-parse", "--verify", "-q", "CHERRY_PICK_HEAD")
	return err == nil
}

// pushBack gives the guest the local branch, the landed commits in place
// of its own, unless its branch moved on since glov fetched sha.
func (r hostRepo) pushBack(vm, branch, sha string) error {
	ref := "refs/heads/" + branch
	_, err := r.git("push", "-q", "--force-with-lease="+ref+":"+sha, project.Remote(vm), ref+":"+ref)
	r.git("update-ref", "-d", landRef(vm, branch))
	r.git("update-ref", "-d", landBase(vm, branch))
	if err != nil {
		r.git("update-ref", landedRef(vm, branch), sha)
		return fmt.Errorf("%s is landed here, but the guest did not take it back (it committed since, or has changes not committed): glov vm land %s again lands what it added\n%v", branch, vm, err)
	}
	r.git("update-ref", "-d", landedRef(vm, branch))
	r.fetch(vm, "")
	fmt.Fprintf(os.Stderr, "%s is yours now, here and in %s: the agent may push it and open the pull request (new commits there need another glov vm land)\n", branch, vm)
	return nil
}

// projectRemoteCheck is what glov vm delete asks before deleting a
// project's VM: whether to drop the guest commits no local branch has.
// It returns the repository to forget the VM in, if there is one.
func projectRemoteCheck(ctx context.Context, c caller, vm string, yes bool) (*hostRepo, error) {
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil || info.Project == nil || info.Project.Sync != api.SyncGit {
		return nil, nil
	}
	if info.State == "running" {
		return nil, fmt.Errorf("VM %s is running: glov vm stop %s first, which fetches its branches", info.Name, info.Name)
	}
	if err := fetchStopped(ctx, c, info, yes); err != nil {
		return nil, err
	}
	repo := &hostRepo{dir: info.Project.Path}
	remote := project.Remote(info.Name)
	refs, err := repo.git("for-each-ref", "--format=%(refname:short)", "refs/remotes/"+remote+"/")
	if err != nil {
		return nil, nil
	}
	var lines []string
	for _, ref := range strings.Fields(refs) {
		base, ok := strings.CutPrefix(ref, remote+"/")
		if !ok || base == "HEAD" {
			continue // the remote's HEAD names one of its branches
		}
		if _, err := repo.git("rev-parse", "--verify", "-q", "refs/heads/"+base); err != nil {
			base = "HEAD"
		}
		if picks, _ := repo.unlanded(base, ref, ""); len(picks) > 0 {
			lines = append(lines, fmt.Sprintf("%s: %s %s lacks", ref, count(len(picks), "commit"), base))
		}
	}
	if len(lines) > 0 {
		fmt.Fprintf(os.Stderr, "%s has guest commits that are on no local branch, gone with the VM:\n%s\n", repo.dir, indent(strings.Join(lines, "\n")))
		if !yes && !confirm("delete them with the VM? [y/N] ") {
			return nil, errors.New("not deleted; glov vm land or git merge brings them in")
		}
	}
	return repo, nil
}

// fetchStopped offers, before glov vm delete, to start stopped project VM
// info and fetch its branches: a VM that Windows or the guest shut down was
// not fetched by glov vm stop, so commits since the last fetch would go
// unlisted. With --yes or without a terminal it only warns. A start or
// fetch that fails stops the delete.
func fetchStopped(ctx context.Context, c caller, info api.VMInfo, yes bool) error {
	unknown := fmt.Sprintf("the guest's commits since %s's last fetch are not listed", info.Name)
	if yes || !isConsole(os.Stdin) {
		fmt.Fprintf(os.Stderr, "warning: %s (glov vm start %s, then glov vm fetch %s, lists them)\n", unknown, info.Name, info.Name)
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s is stopped, so %s\n", info.Name, unknown)
	if a := strings.ToLower(strings.TrimSpace(ask("start it to fetch them first? [Y/n] "))); a != "" && a != "y" && a != "yes" {
		return nil
	}
	fmt.Fprintf(os.Stderr, "starting %s\n", info.Name)
	if err := call(ctx, c, api.MethodVMStart, api.VMParams{VM: info.Name}, &info); err != nil {
		return err
	}
	err := call(ctx, c, api.MethodVMWait, api.WaitParams{VM: info.Name, State: api.WaitGuestReady, Timeout: agentReady.Seconds()}, &info)
	if err == nil {
		err = projectFetch(ctx, c, info)
	}
	fmt.Fprintf(os.Stderr, "stopping %s\n", info.Name)
	if stopErr := call(ctx, c, api.MethodVMStop, api.StopParams{VM: info.Name}, &info); err == nil {
		err = stopErr
	}
	if err != nil {
		return fmt.Errorf("not deleted: %w (glov vm delete %s --yes deletes it without the fetch)", err, info.Name)
	}
	fmt.Fprintf(os.Stderr, "fetched %s's branches into %s\n", info.Name, info.Project.Path)
	return nil
}

// forget removes the VM's remote (its remote-tracking branches with it) and
// land state from the project.
func (r hostRepo) forget(vm string) {
	r.git("remote", "remove", project.Remote(vm))
	if refs, err := r.git("for-each-ref", "--format=%(refname)", "refs/glov-land/"+vm+"/"); err == nil {
		for _, ref := range strings.Fields(refs) {
			r.git("update-ref", "-d", ref)
		}
	}
}

// count is n things, in the plural unless one.
func count(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

func indent(s string) string {
	if s == "" {
		return s
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	return s[strings.LastIndexByte(s, '\n')+1:]
}

func or(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
