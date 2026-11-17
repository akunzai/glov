package project

import (
	"os"
	"path/filepath"
	"strings"
)

// The live shares of a project's VM: the project itself, and a worktree's
// repository, whose git data the worktree's .git names (as on macOS).
const (
	ShareName    = "project"
	GitShareName = "project-git"
)

// Worktree is a git worktree's repository: Common is the repository's git
// directory, Dir the worktree's own directory inside it (worktrees/<name>).
type Worktree struct {
	Common, Dir string
}

// GitDirectory is the repository of the worktree at project: its .git is
// a file (not a link) naming a directory under <git dir>/worktrees, which
// names project's .git back, of a git directory with HEAD, objects and
// refs. ok is false for anything else, a project's own .git directory
// included.
func GitDirectory(project string) (Worktree, bool) {
	dotGit := filepath.Join(project, ".git")
	if fi, err := os.Lstat(dotGit); err != nil || !fi.Mode().IsRegular() {
		return Worktree{}, false
	}
	text, err := os.ReadFile(dotGit)
	if err != nil {
		return Worktree{}, false
	}
	line, _, _ := strings.Cut(string(text), "\n")
	named, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return Worktree{}, false
	}
	named = filepath.FromSlash(strings.TrimSpace(named))
	if !filepath.IsAbs(named) {
		named = filepath.Join(project, named)
	}
	dir, err := filepath.EvalSymlinks(named)
	if err != nil {
		return Worktree{}, false
	}
	parent := filepath.Dir(dir)
	if filepath.Base(parent) != "worktrees" {
		return Worktree{}, false
	}
	common := filepath.Dir(parent)
	// The worktree's directory names the project's .git back.
	back, err := os.ReadFile(filepath.Join(dir, "gitdir"))
	if err != nil {
		return Worktree{}, false
	}
	b, err1 := filepath.EvalSymlinks(filepath.FromSlash(strings.TrimSpace(string(back))))
	d, err2 := filepath.EvalSymlinks(dotGit)
	if err1 != nil || err2 != nil || !strings.EqualFold(b, d) {
		return Worktree{}, false
	}
	for _, e := range []struct {
		name string
		dir  bool
	}{{"HEAD", false}, {"objects", true}, {"refs", true}} {
		fi, err := os.Stat(filepath.Join(common, e.name))
		if err != nil || fi.IsDir() != e.dir {
			return Worktree{}, false
		}
	}
	return Worktree{Common: common, Dir: dir}, true
}
