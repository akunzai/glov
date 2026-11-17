package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAWorktreeNamesItsRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	repo, wt := filepath.Join(dir, "repo"), filepath.Join(dir, "wt")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", repo)
	git("-C", repo, "commit", "-q", "--allow-empty", "-m", "first")
	git("-C", repo, "worktree", "add", "-q", wt)
	w, ok := GitDirectory(wt)
	common, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if !ok || w.Common != common || w.Dir != filepath.Join(common, "worktrees", "wt") {
		t.Errorf("worktree %+v %v, want common %s", w, ok, common)
	}
	if _, ok := GitDirectory(repo); ok {
		t.Error("a repository's own .git is no worktree")
	}
	// A .git file naming a directory that does not name it back.
	other := filepath.Join(dir, "other")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, ".git"), []byte("gitdir: "+w.Dir+"\n"), 0o644)
	if _, ok := GitDirectory(other); ok {
		t.Error("a .git file the worktree does not name back")
	}
}
