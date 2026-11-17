package live

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// memSide is a side in memory: files by path (content, mtime), directories.
type memSide struct {
	name    string
	files   map[string]string
	mtimes  map[string]int64
	dirs    map[string]bool
	other   map[string]bool
	fail    map[string]error // what the next change of a path fails with
	pending map[string]error
	ops     []string
}

func newSide(name string) *memSide {
	return &memSide{name: name, files: map[string]string{}, mtimes: map[string]int64{}, dirs: map[string]bool{},
		other: map[string]bool{}, fail: map[string]error{}, pending: map[string]error{}}
}

func (m *memSide) Name() string { return m.name }

func (m *memSide) state(p string) (State, bool) {
	switch {
	case m.other[p]:
		return State{Other: true}, true
	case m.dirs[p]:
		return State{Dir: true}, true
	}
	c, ok := m.files[p]
	return State{Hash: sum(c), Mtime: m.mtimes[p]}, ok
}

func (m *memSide) List(scope string) (Snapshot, error) {
	out := Snapshot{}
	all := slices.Concat(slices.Collect(maps.Keys(m.files)), slices.Collect(maps.Keys(m.dirs)), slices.Collect(maps.Keys(m.other)))
	for _, p := range all {
		if scope == "" || strings.HasPrefix(p, scope+"/") {
			out[p], _ = m.state(p)
		}
	}
	return out, nil
}

func (m *memSide) Stat(paths []string) (Snapshot, error) {
	out := Snapshot{}
	for _, p := range paths {
		if st, ok := m.state(p); ok {
			out[p] = st
		}
	}
	return out, nil
}

func (m *memSide) Open(p string) (io.ReadCloser, error) {
	c, ok := m.files[p]
	if !ok {
		return nil, fmt.Errorf("%s: no file", p)
	}
	return io.NopCloser(strings.NewReader(c)), nil
}

func (m *memSide) change(p, op string) error {
	m.ops = append(m.ops, op+" "+p)
	if err := m.fail[p]; err != nil {
		delete(m.fail, p)
		m.pending[p] = err
		return errDeferred
	}
	return nil
}

var errDeferred = errors.New("deferred")

func (m *memSide) holds(p, expect string) bool {
	c, ok := m.files[p]
	return expect == "" && !ok || ok && sum(c) == expect
}

func (m *memSide) Put(p string, r io.Reader, hash, expect string) error {
	b, _ := io.ReadAll(r)
	if err := m.change(p, "put"); err != nil {
		return nil // reported by Flush, as the guest side does
	}
	if !m.holds(p, expect) {
		m.pending[p] = ErrConflict
		return nil
	}
	if sum(string(b)) != hash {
		return ErrChanged
	}
	m.files[p] = string(b)
	return nil
}

func (m *memSide) Remove(p, expect string) error {
	if err := m.change(p, "remove"); err != nil {
		return nil
	}
	if !m.holds(p, expect) {
		return ErrConflict
	}
	delete(m.files, p)
	return nil
}

func (m *memSide) Mkdir(p string) error {
	if err := m.change(p, "mkdir"); err != nil {
		return nil
	}
	m.dirs[p] = true
	return nil
}

func (m *memSide) Rmdir(p string) error {
	if err := m.change(p, "rmdir"); err != nil {
		return nil
	}
	for q := range m.files {
		if strings.HasPrefix(q, p+"/") {
			return nil
		}
	}
	delete(m.dirs, p)
	return nil
}

func (m *memSide) Flush() map[string]error {
	out := m.pending
	m.pending = map[string]error{}
	return out
}

type pair struct {
	t           *testing.T
	host, guest *memSide
	e           *Engine
}

func newPair(t *testing.T) *pair {
	h, g := newSide("host"), newSide("guest")
	return &pair{t, h, g, &Engine{Host: h, Guest: g}}
}

func (p *pair) round(full bool, paths ...string) Result {
	p.t.Helper()
	r, err := p.e.Round(full, paths)
	if err != nil {
		p.t.Fatal(err)
	}
	return r
}

// inStep checks that both sides hold the same files and directories.
func (p *pair) inStep() {
	p.t.Helper()
	if !maps.Equal(p.host.files, p.guest.files) || !maps.Equal(p.host.dirs, p.guest.dirs) {
		p.t.Fatalf("out of step:\nhost  %v %v\nguest %v %v", p.host.files, p.host.dirs, p.guest.files, p.guest.dirs)
	}
}

func TestTheFirstRoundCopiesEachSideToTheOther(t *testing.T) {
	p := newPair(t)
	p.host.files["src/a.go"], p.host.dirs["src"] = "a", true
	p.guest.files["out.log"] = "log"
	r := p.round(true)
	p.inStep()
	if r.ToGuest != 2 || r.ToHost != 1 || len(r.Conflicts) != 0 {
		t.Errorf("result %+v", r)
	}
	// Directories are made before the files in them.
	if i, j := slices.Index(p.guest.ops, "mkdir src"), slices.Index(p.guest.ops, "put src/a.go"); i < 0 || i > j {
		t.Errorf("guest ops %v", p.guest.ops)
	}
	if r := p.round(true); r.ToGuest+r.ToHost != 0 {
		t.Errorf("a second round changed %+v", r)
	}
}

func TestAChangeOnOneSideReachesTheOther(t *testing.T) {
	p := newPair(t)
	p.host.files["a"], p.host.files["b"] = "1", "1"
	p.round(true)
	p.guest.files["a"] = "guest edit"
	delete(p.host.files, "b")
	p.round(false, "a", "b")
	p.inStep()
	if p.host.files["a"] != "guest edit" {
		t.Errorf("host a = %q", p.host.files["a"])
	}
	if _, ok := p.e.Base["b"]; ok {
		t.Error("b still in the base")
	}
}

func TestBothSidesChangedTheNewerWins(t *testing.T) {
	for _, c := range []struct {
		name               string
		hostMtime, guestMt int64
		want, winner       string
	}{
		{"guest newer", 1, 2, "guest", "guest"},
		{"host newer", 2, 1, "host", "host"},
		{"tie", 1, 1, "host", "host"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPair(t)
			p.host.files["a"] = "base"
			p.round(true)
			p.host.files["a"], p.host.mtimes["a"] = "host", c.hostMtime
			p.guest.files["a"], p.guest.mtimes["a"] = "guest", c.guestMt
			r := p.round(false, "a")
			p.inStep()
			if p.host.files["a"] != c.want || len(r.Conflicts) != 1 || r.Conflicts[0].Winner != c.winner {
				t.Errorf("a = %q, conflicts %+v", p.host.files["a"], r.Conflicts)
			}
		})
	}
}

func TestAChangeBeatsADeletion(t *testing.T) {
	p := newPair(t)
	p.host.files["a"], p.host.files["b"] = "1", "1"
	p.round(true)
	delete(p.host.files, "a")
	p.guest.files["a"] = "edited"
	p.host.files["b"] = "edited"
	delete(p.guest.files, "b")
	r := p.round(true)
	p.inStep()
	if p.host.files["a"] != "edited" || p.host.files["b"] != "edited" || len(r.Conflicts) != 2 {
		t.Errorf("files %v, conflicts %+v", p.host.files, r.Conflicts)
	}
}

func TestAMovedDirectoryMovesWithItsFiles(t *testing.T) {
	p := newPair(t)
	p.guest.dirs["old"], p.guest.dirs["old/sub"] = true, true
	p.guest.files["old/sub/x"], p.guest.files["old/y"] = "x", "y"
	p.round(true)
	// The guest reports just the two directory names, as inotify does.
	for _, q := range []string{"old", "old/sub"} {
		delete(p.guest.dirs, q)
		p.guest.dirs["new"+strings.TrimPrefix(q, "old")] = true
	}
	for q, c := range maps.Clone(p.guest.files) {
		delete(p.guest.files, q)
		p.guest.files["new"+strings.TrimPrefix(q, "old")] = c
	}
	p.round(false, "old", "new")
	p.inStep()
	if p.host.files["new/sub/x"] != "x" || p.host.dirs["old"] {
		t.Errorf("host %v %v", p.host.files, p.host.dirs)
	}
	// Removed deepest first, after the files.
	if i, j := slices.Index(p.host.ops, "rmdir old/sub"), slices.Index(p.host.ops, "rmdir old"); i < 0 || i > j {
		t.Errorf("host ops %v", p.host.ops)
	}
}

func TestAFailedChangeIsRetriedAndKeepsTheBase(t *testing.T) {
	p := newPair(t)
	p.host.files["a"] = "1"
	p.guest.fail["a"] = errors.New("in use")
	r := p.round(true)
	if r.Retry["a"] == nil || r.ToGuest != 0 {
		t.Fatalf("result %+v", r)
	}
	if _, ok := p.e.Base["a"]; ok {
		t.Error("a failed write is in the base")
	}
	p.round(false, "a")
	p.inStep()
}

func TestAConflictingDestinationIsLeftAlone(t *testing.T) {
	p := newPair(t)
	p.host.files["a"] = "1"
	p.round(true)
	p.host.files["a"] = "2"
	// The guest changes a after the round looked: the guest side refuses.
	p.guest.fail["a"] = ErrConflict
	r := p.round(false, "a")
	if !errors.Is(r.Retry["a"], ErrConflict) {
		t.Fatalf("retry %v", r.Retry)
	}
}

func TestLinksAreNeverTouched(t *testing.T) {
	p := newPair(t)
	p.host.other["link"] = true
	p.guest.files["link"] = "guest file"
	r := p.round(true)
	if !slices.Equal(r.Skipped, []string{"link"}) || len(p.host.ops)+len(p.guest.ops) != 0 {
		t.Errorf("result %+v, ops %v %v", r, p.host.ops, p.guest.ops)
	}
}

func TestStateIsKeptOnlyForTheSameFolderRootAndDisk(t *testing.T) {
	file := filepath.Join(t.TempDir(), "s.live.json")
	base := Snapshot{"a.txt": {Hash: "h"}}
	if err := Save(file, `C:\src\p`, "/mnt/c/src/p", 1, base, map[string]HostKey{"a.txt": {Key: "k", Hash: "h"}}); err != nil {
		t.Fatal(err)
	}
	if got, keys, err := Load(file, `c:\SRC\p`, "/mnt/c/src/p", 1); err != nil || len(got) != 1 || keys["a.txt"].Hash != "h" {
		t.Errorf("same pair = %v, %v, %v", got, keys, err)
	}
	// glov vm reset gave the VM an older disk: the base would delete what
	// the host gained since.
	for _, c := range []struct {
		host, guest string
		disk        int
	}{{`C:\src\p`, "/mnt/c/src/p", 2}, {`C:\src\q`, "/mnt/c/src/p", 1}, {`C:\src\p`, "/mnt/x", 1}} {
		if got, _, err := Load(file, c.host, c.guest, c.disk); err != nil || len(got) != 0 {
			t.Errorf("Load %+v = %v, %v", c, got, err)
		}
	}
}

func TestGitAtWorkOnOneSideHoldsItsDirectory(t *testing.T) {
	p := newPair(t)
	p.e.GitDirs = []string{".git"}
	p.host.files[".git/HEAD"], p.host.files["a.go"] = "ref: refs/heads/main", "a"
	p.round(true)
	p.guest.files[".git/index.lock"] = "half"
	p.guest.files[".git/index"], p.guest.files["a.go"] = "new", "guest edit"
	r := p.round(false, ".git/index", "a.go")
	if _, ok := p.host.files[".git/index"]; ok || !errors.Is(r.Retry[".git/index"], ErrBusy) {
		t.Errorf("index went while git was at work: %+v", r)
	}
	if p.host.files["a.go"] != "guest edit" {
		t.Error("the work tree waited for git too")
	}
	delete(p.guest.files, ".git/index.lock")
	r = p.round(false, ".git/index")
	if p.host.files[".git/index"] != "new" || len(r.Retry) != 0 {
		t.Errorf("index once git was done: %+v", r)
	}
	if _, ok := p.host.files[".git/index.lock"]; ok {
		t.Error("the lock file was copied")
	}
	// A lock seen once holds the directory in the rounds after, whatever
	// they look at.
	p.guest.files[".git/refs/heads/main.lock"] = "1"
	p.round(false, ".git/refs/heads/main.lock")
	p.guest.files[".git/HEAD"] = "ref: refs/heads/work"
	if r := p.round(false, ".git/HEAD"); !errors.Is(r.Retry[".git/HEAD"], ErrBusy) {
		t.Errorf("HEAD went while git was at work: %+v", r)
	}
	delete(p.guest.files, ".git/refs/heads/main.lock")
	if r := p.round(false, ".git/HEAD"); p.host.files[".git/HEAD"] != "ref: refs/heads/work" || len(r.Locks) != 0 {
		t.Errorf("HEAD once git was done: %+v", r)
	}
}

func TestAFullRoundSeesAnyLockInTheGitDirectory(t *testing.T) {
	p := newPair(t)
	p.e.GitDirs = []string{""}
	p.host.files["refs/heads/main"] = "1"
	p.round(true)
	p.host.files["refs/heads/main.lock"], p.host.files["refs/heads/main"] = "2", "2"
	if r := p.round(true); p.guest.files["refs/heads/main"] != "1" || !errors.Is(r.Retry["refs/heads/main"], ErrBusy) {
		t.Errorf("guest main = %q, %+v", p.guest.files["refs/heads/main"], r)
	}
}

func TestABranchBothSidesMovedKeepsTheLosersAsAnother(t *testing.T) {
	p := newPair(t)
	p.e.GitDirs = []string{".git"}
	p.host.files[".git/refs/heads/main"] = "base"
	p.round(true)
	p.host.files[".git/refs/heads/main"], p.host.mtimes[".git/refs/heads/main"] = "host", 2
	p.guest.files[".git/refs/heads/main"], p.guest.mtimes[".git/refs/heads/main"] = "guest", 1
	r := p.round(false, ".git/refs/heads/main")
	p.inStep()
	kept := ".git/refs/heads/main-glov-conflict-guest"
	if p.host.files[".git/refs/heads/main"] != "host" || p.host.files[kept] != "guest" {
		t.Errorf("host %v", p.host.files)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].Kept != "main-glov-conflict-guest" {
		t.Errorf("conflicts %+v", r.Conflicts)
	}
	// A second conflict replaces the branch kept before.
	p.host.files[".git/refs/heads/main"], p.host.mtimes[".git/refs/heads/main"] = "host 2", 3
	p.guest.files[".git/refs/heads/main"], p.guest.mtimes[".git/refs/heads/main"] = "guest 2", 2
	r = p.round(false, ".git/refs/heads/main")
	p.inStep()
	if p.host.files[".git/refs/heads/main"] != "host 2" || p.host.files[kept] != "guest 2" || len(r.Retry) != 0 {
		t.Errorf("host %v, %+v", p.host.files, r)
	}
	if r := p.round(true); r.ToGuest+r.ToHost != 0 || len(r.Conflicts) != 0 {
		t.Errorf("a round after changed %+v", r)
	}
}
