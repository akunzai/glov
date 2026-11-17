// Package live keeps a live share's two sides in step: the host folder and
// its copy in the guest's own file system. Each round compares, path by path, what each
// side holds now with what both held after the last round (the base): a
// change on one side is carried to the other, and when both changed the
// newer one wins and the conflict is reported. The sides are behind Side, so
// the decisions are tested without a VM.
package live

import (
	"cmp"
	"errors"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
)

// State is what a side holds at a path: a directory, a file with its
// SHA-256 and last write time (Unix nanoseconds), or something a sync never
// touches (a link, a junction, a special or unreadable file).
type State struct {
	Dir   bool
	Other bool
	Hash  string
	Mtime int64
}

// same reports whether a and b hold the same thing (nil: nothing).
func same(a, b *State) bool {
	switch {
	case a == nil || b == nil:
		return a == nil && b == nil
	case a.Dir || b.Dir:
		return a.Dir && b.Dir
	}
	return !a.Other && !b.Other && a.Hash == b.Hash
}

// Snapshot is a side's states by path ('/'-separated, relative to the root).
type Snapshot map[string]State

func (s Snapshot) at(p string) *State {
	if st, ok := s[p]; ok {
		return &st
	}
	return nil
}

// Errors a Side reports for one path; the engine tries the path again.
var (
	// ErrConflict: the destination is not what the round saw, so it was
	// left alone (someone changed it meanwhile).
	ErrConflict = errors.New("changed since the round looked at it")
	// ErrChanged: the source changed while it was sent.
	ErrChanged = errors.New("the source changed while it was sent")
	// ErrBusy: git is at work on one side (a *.lock file in its
	// directory), so the path waits; it does not count as a failure.
	ErrBusy = errors.New("git is at work on one side")
	// ErrName: the path is not one a Windows folder can hold as itself (see
	// checkName), so the host refuses it untouched; it is not retried.
	ErrName = errors.New("not a name a Windows folder can hold as itself")
	// ErrExcluded: the path, or a directory above it, is one of the
	// share's --exclude paths, which stay out of the sync on both sides, so
	// the host refuses it untouched; it is not retried.
	ErrExcluded = errors.New("excluded from the share")
)

// Side is one side of a share. Paths are relative to its root. Changes may
// be applied at once or batched: Flush applies what is pending and returns
// what failed, by path.
type Side interface {
	Name() string
	// List returns scope's contents below it, scope itself excluded (all
	// of the root for ""), leaving out excluded paths.
	List(scope string) (Snapshot, error)
	// Stat returns the states of paths; a path absent from the result is
	// absent from the side (excluded ones are left out too).
	Stat(paths []string) (Snapshot, error)
	// Open reads a file whose state was hash.
	Open(p string) (io.ReadCloser, error)
	// Put makes p a file with content r, whose SHA-256 must be hash,
	// replacing expect (a hash, or "" for nothing).
	Put(p string, r io.Reader, hash, expect string) error
	// Remove deletes the file p, which must hold expect.
	Remove(p, expect string) error
	Mkdir(p string) error
	// Rmdir removes the directory p if empty; a non-empty one is kept.
	Rmdir(p string) error
	Flush() map[string]error
}

// Conflict is a path both sides changed since the last round.
type Conflict struct {
	Path string
	// Winner is the side whose version was kept: "host" or "guest".
	Winner string
	Why    string
	// Kept is the branch the other side's version of a branch ref was
	// kept as, on both sides.
	Kept string
}

// Engine runs rounds over two sides. Host wins ties.
type Engine struct {
	Host, Guest Side
	// Base is what both held after the last round; Save persists it.
	Base Snapshot
	// GitDirs are git directories in the share ("" for the root, ".git"),
	// which two sides' git must not change at once: their *.lock files are
	// never copied, and while either side holds one, the rest of that
	// directory waits (ErrBusy). A branch ref both sides changed keeps the
	// loser's as another branch (Conflict.Kept).
	GitDirs []string

	// held are the lock files in GitDirs the last round saw: a round that
	// only looks at changed paths looks at them again, until they go.
	held map[string]bool
}

// gitLocks are the lock files git holds in its directory's top level
// while it changes the index, HEAD or the refs, which a round that only
// looks at changed paths might not see.
var gitLocks = []string{"index.lock", "HEAD.lock", "packed-refs.lock", "config.lock", "shallow.lock", "ORIG_HEAD.lock"}

// gitDir is the GitDirs entry p is in, if any.
func (e *Engine) gitDir(p string) (string, bool) {
	for _, g := range e.GitDirs {
		if g == "" || p == g || strings.HasPrefix(p, g+"/") {
			return g, true
		}
	}
	return "", false
}

func within(g, p string) string {
	if g == "" {
		return p
	}
	return g + "/" + p
}

// Result is what a round did.
type Result struct {
	ToGuest, ToHost int
	Conflicts       []Conflict
	// Retry are the paths whose change failed this round, with why.
	Retry map[string]error
	// Skipped are paths one side holds something a sync never touches at.
	Skipped []string
	// Locks are the *.lock files in GitDirs, as "<side> <path>", that hold
	// their directory (ErrBusy).
	Locks []string
}

type action struct {
	path string
	// src is the path on from, if not path.
	src      string
	from, to Side
	want     *State // what to make path on to: nil removes it
	have     *State // what to currently holds
	wantBase *State // the base once done
}

// Round reconciles: everything with full; otherwise paths (a directory
// whose kind differs anywhere is reconciled with what lies below it).
func (e *Engine) Round(full bool, paths []string) (Result, error) {
	if e.Base == nil {
		e.Base = Snapshot{}
	}
	host, guest := Snapshot{}, Snapshot{}
	cands := map[string]bool{}
	if full {
		var err error
		if host, err = e.Host.List(""); err != nil {
			return Result{}, err
		}
		if guest, err = e.Guest.List(""); err != nil {
			return Result{}, err
		}
		for p := range e.Base {
			cands[p] = true
		}
	} else {
		paths = cleanPaths(paths)
		if err := e.stat(paths, host, guest); err != nil {
			return Result{}, err
		}
		for _, p := range paths {
			cands[p] = true
			h, g, b := host.at(p), guest.at(p), e.Base.at(p)
			if !anyDir(h, g, b) || allDirs(h, g, b) {
				continue
			}
			if err := e.subtree(p, host, guest, cands); err != nil {
				return Result{}, err
			}
		}
	}
	for p := range host {
		cands[p] = true
	}
	for p := range guest {
		cands[p] = true
	}
	var res Result
	busy, locks, err := e.busyGitDirs(full, host, guest)
	if err != nil {
		return Result{}, err
	}
	res.Locks = locks
	var acts []action
	for _, p := range slices.Sorted(maps.Keys(cands)) {
		if gd, ok := e.gitDir(p); ok {
			if strings.HasSuffix(p, ".lock") {
				continue // git's own, for the side that holds it
			}
			if busy[gd] {
				if res.Retry == nil {
					res.Retry = map[string]error{}
				}
				res.Retry[p] = ErrBusy
				continue
			}
		}
		h, g, b := host.at(p), guest.at(p), e.Base.at(p)
		if h != nil && h.Other || g != nil && g.Other {
			res.Skipped = append(res.Skipped, p)
			continue
		}
		if same(h, g) {
			e.setBase(p, h)
			continue
		}
		hostChanged, guestChanged := !same(h, b), !same(g, b)
		toGuest := !guestChanged
		if hostChanged && guestChanged {
			c := Conflict{Path: p}
			toGuest, c.Why = hostWins(h, g)
			c.Winner = "guest"
			if toGuest {
				c.Winner = "host"
			}
			if kept, a := e.keepBranch(p, h, g, toGuest); kept != "" {
				c.Kept = kept
				acts = append(acts, a...)
			}
			res.Conflicts = append(res.Conflicts, c)
		}
		if toGuest {
			acts = append(acts, action{path: p, from: e.Host, to: e.Guest, want: h, have: g, wantBase: h})
		} else {
			acts = append(acts, action{path: p, from: e.Guest, to: e.Host, want: g, have: h, wantBase: g})
		}
	}
	if err := e.keptHave(acts, host, guest); err != nil {
		return Result{}, err
	}
	for p, err := range e.run(acts) {
		if res.Retry == nil {
			res.Retry = map[string]error{}
		}
		res.Retry[p] = err
	}
	for _, a := range acts {
		if res.Retry[a.path] != nil {
			continue
		}
		if a.to == e.Guest {
			res.ToGuest++
		} else {
			res.ToHost++
		}
	}
	return res, nil
}

// busyGitDirs are the GitDirs with a *.lock file on either side, and
// those files.
func (e *Engine) busyGitDirs(full bool, host, guest Snapshot) (map[string]bool, []string, error) {
	if len(e.GitDirs) == 0 {
		return nil, nil, nil
	}
	if !full {
		paths := slices.Collect(maps.Keys(e.held))
		for _, g := range e.GitDirs {
			for _, l := range gitLocks {
				paths = append(paths, within(g, l))
			}
		}
		if err := e.stat(paths, host, guest); err != nil {
			return nil, nil, err
		}
	}
	busy, held := map[string]bool{}, map[string]bool{}
	var locks []string
	for _, s := range []struct {
		name string
		snap Snapshot
	}{{"host", host}, {"guest", guest}} {
		for p, st := range s.snap {
			if g, ok := e.gitDir(p); ok && !st.Dir && strings.HasSuffix(p, ".lock") {
				busy[g], held[p] = true, true
				locks = append(locks, s.name+" "+p)
			}
		}
	}
	slices.Sort(locks)
	e.held = held
	return busy, locks, nil
}

// keepBranch, for a branch ref (refs/heads/...) both sides changed, keeps
// the losing side's as <branch>-glov-conflict-<side> on both sides, so no
// commits are lost: it returns that branch and the actions making it.
func (e *Engine) keepBranch(p string, h, g *State, hostWon bool) (string, []action) {
	gd, ok := e.gitDir(p)
	if !ok || h == nil || g == nil || h.Dir || g.Dir || h.Other || g.Other {
		return "", nil
	}
	branch, ok := strings.CutPrefix(p, within(gd, "refs/heads/"))
	if !ok {
		return "", nil
	}
	loser, side, st := e.Guest, "guest", g
	if !hostWon {
		loser, side, st = e.Host, "host", h
	}
	kept := p + "-glov-conflict-" + side
	// These come before the action replacing p, so they read the loser's
	// p first.
	return branch + "-glov-conflict-" + side, []action{
		{path: kept, src: p, from: loser, to: e.Host, want: st, wantBase: st},
		{path: kept, src: p, from: loser, to: e.Guest, want: st, wantBase: st},
	}
}

// keptHave fills in what the branches keepBranch makes hold now: a
// conflict kept before has made them already.
func (e *Engine) keptHave(acts []action, host, guest Snapshot) error {
	var paths []string
	for _, a := range acts {
		if a.src != "" {
			paths = append(paths, a.path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	if err := e.stat(paths, host, guest); err != nil {
		return err
	}
	for i, a := range acts {
		if a.src == "" {
			continue
		}
		if a.to == e.Host {
			acts[i].have = host.at(a.path)
		} else {
			acts[i].have = guest.at(a.path)
		}
	}
	return nil
}

// hostWins decides a conflict: a change beats a deletion, a directory beats
// a file, otherwise the newer file wins, the host on a tie.
func hostWins(h, g *State) (bool, string) {
	switch {
	case h == nil:
		return false, "deleted on the host, changed in the guest"
	case g == nil:
		return true, "changed on the host, deleted in the guest"
	case h.Dir != g.Dir:
		return h.Dir, "a directory on one side, a file on the other"
	case g.Mtime > h.Mtime:
		return false, "changed on both sides; the guest's is newer"
	}
	return true, "changed on both sides; the host's is newer"
}

func (e *Engine) setBase(p string, st *State) {
	if st == nil {
		delete(e.Base, p)
		return
	}
	e.Base[p] = State{Dir: st.Dir, Hash: st.Hash}
}

func (e *Engine) stat(paths []string, host, guest Snapshot) error {
	h, err := e.Host.Stat(paths)
	if err != nil {
		return err
	}
	g, err := e.Guest.Stat(paths)
	if err != nil {
		return err
	}
	for p, st := range h {
		host[p] = st
	}
	for p, st := range g {
		guest[p] = st
	}
	return nil
}

// subtree adds what lies below the directory p on either side or in the base.
func (e *Engine) subtree(p string, host, guest Snapshot, cands map[string]bool) error {
	for _, s := range []struct {
		side Side
		snap Snapshot
	}{{e.Host, host}, {e.Guest, guest}} {
		if st := s.snap.at(p); st == nil || !st.Dir {
			continue
		}
		list, err := s.side.List(p)
		if err != nil {
			return err
		}
		for q, st := range list {
			s.snap[q] = st
		}
	}
	for q := range e.Base {
		if strings.HasPrefix(q, p+"/") {
			cands[q] = true
		}
	}
	return nil
}

// run carries out acts: file removals, then directories removed deepest
// first, made shallowest first, then files written. It returns what failed.
func (e *Engine) run(acts []action) map[string]error {
	kind := func(a action) int {
		switch {
		case a.want == nil && (a.have == nil || !a.have.Dir):
			return 0
		case a.want == nil:
			return 1
		case a.want.Dir:
			return 2
		}
		return 3
	}
	slices.SortStableFunc(acts, func(a, b action) int {
		if c := cmp.Compare(kind(a), kind(b)); c != 0 {
			return c
		}
		if kind(a) == 1 { // deepest first
			return cmp.Compare(strings.Count(b.path, "/"), strings.Count(a.path, "/"))
		}
		return cmp.Compare(strings.Count(a.path, "/"), strings.Count(b.path, "/"))
	})
	failed := map[string]error{}
	for _, a := range acts {
		if err := e.do(a); err != nil {
			failed[a.path] = err
		}
	}
	for _, s := range []Side{e.Host, e.Guest} {
		for p, err := range s.Flush() {
			failed[p] = err
		}
	}
	for _, a := range acts {
		if failed[a.path] == nil {
			e.setBase(a.path, a.wantBase)
		}
	}
	return failed
}

func (e *Engine) do(a action) error {
	expect := ""
	if a.have != nil && !a.have.Dir {
		expect = a.have.Hash
	}
	switch {
	case a.want == nil && a.have != nil && a.have.Dir:
		return a.to.Rmdir(a.path)
	case a.want == nil:
		return a.to.Remove(a.path, expect)
	case a.want.Dir:
		if a.have != nil && !a.have.Dir {
			if err := a.to.Remove(a.path, expect); err != nil {
				return err
			}
		}
		return a.to.Mkdir(a.path)
	}
	if a.have != nil && a.have.Dir {
		if err := a.to.Rmdir(a.path); err != nil {
			return err
		}
	}
	r, err := a.from.Open(cmp.Or(a.src, a.path))
	if err != nil {
		return err
	}
	defer r.Close()
	return a.to.Put(a.path, r, a.want.Hash, expect)
}

func anyDir(s ...*State) bool {
	return slices.ContainsFunc(s, func(st *State) bool { return st != nil && st.Dir })
}

func allDirs(s ...*State) bool {
	return !slices.ContainsFunc(s, func(st *State) bool { return st == nil || !st.Dir })
}

func cleanPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if p = path.Clean(p); p != "." && p != "" && !strings.HasPrefix(p, "../") && p != ".." {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
