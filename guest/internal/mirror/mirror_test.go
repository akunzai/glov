package mirror_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/internal/mirror"
	"github.com/akunzai/glov/guest/proto"
)

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type fixture struct {
	t     *testing.T
	m     *mirror.Manager
	root  string
	state string
}

func newFixture(t *testing.T) *fixture {
	// Resolved: the share root may not lie below a link, and on macOS the
	// temporary directory does (/var is one).
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state")
	return &fixture{t, mirror.NewManager(state, func(d string) error { return os.MkdirAll(d, 0o755) }), filepath.Join(base, "root"), state}
}

func (f *fixture) put(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(rel)))
	return string(b), err == nil
}

// manifest returns every entry by path, following pages.
func (f *fixture) manifest(p mirror.ManifestParams) (map[string]mirror.Entry, mirror.ManifestResult) {
	f.t.Helper()
	p.ID, p.Root = "proj", f.root
	all := map[string]mirror.Entry{}
	var first mirror.ManifestResult
	for {
		r, err := f.m.Manifest(p)
		if err != nil {
			f.t.Fatal(err)
		}
		if p.Offset == 0 {
			first = r
		}
		for _, e := range r.Entries {
			all[e.Path] = e
		}
		p.Offset += len(r.Entries)
		if p.Offset >= r.Total {
			return all, first
		}
	}
}

func (f *fixture) apply(commit bool, ops ...mirror.Op) []mirror.Failure {
	f.t.Helper()
	r, err := f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Ops: ops, Commit: commit})
	if err != nil {
		f.t.Fatal(err)
	}
	return r.Failures
}

func write(path, content string) mirror.Op {
	return mirror.Op{Op: "write", Path: path, Data: []byte(content), End: true, Hash: sum(content)}
}

// tempFiles lists the sync's temporary files anywhere below dir.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(d.Name(), ".glov-sync") {
			found = append(found, path)
		}
		return nil
	})
	return found
}

func keys(m map[string]mirror.Entry) []string {
	return slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	})
}

func TestFirstManifestHashesEverythingAndSkipsExcludedPaths(t *testing.T) {
	f := newFixture(t)
	f.put("a.txt", "a")
	f.put("src/b.cs", "bb")
	f.put("bin/x.dll", "x")
	f.put("src/bin/y.dll", "y")
	f.put("keep.log", "k")
	f.put("drop.log", "d")
	f.put("root-only/z", "z")
	f.put("src/root-only/w", "w")
	if runtime.GOOS != "windows" {
		os.Symlink("/etc", filepath.Join(f.root, "link"))
	}
	rules := []mirror.Rule{
		{Pattern: `^(?:.*/)?bin$`, DirOnly: true},
		{Pattern: `^(?:.*/)?[^/]*\.log$`},
		{Pattern: `^(?:.*/)?keep\.log$`, Negate: true},
		{Pattern: `^root-only$`},
	}
	got, r := f.manifest(mirror.ManifestParams{Exclude: rules})
	want := []string{"a.txt", "keep.log", "src", "src/b.cs", "src/root-only", "src/root-only/w"}
	if !slices.Equal(keys(got), want) {
		t.Fatalf("paths = %v, want %v", keys(got), want)
	}
	if !r.Fresh || r.Hashed != 4 {
		t.Fatalf("fresh %v hashed %d, want a fresh manifest and 4 files hashed", r.Fresh, r.Hashed)
	}
	if e := got["src/b.cs"]; e.Hash != sum("bb") || e.Size != 2 || e.Dir || e.Modified {
		t.Fatalf("src/b.cs = %+v", e)
	}
	if !got["src"].Dir || got["src"].Hash != "" {
		t.Fatalf("src = %+v", got["src"])
	}
}

func TestApplyThenManifestReusesHashesUntilTheStatKeyChanges(t *testing.T) {
	f := newFixture(t)
	f.put("old.txt", "old")
	f.put("stale/gone.txt", "g")
	f.manifest(mirror.ManifestParams{})
	fails := f.apply(true,
		mirror.Op{Op: "delete", Path: "stale/gone.txt"}, mirror.Op{Op: "rmdir", Path: "stale"},
		mirror.Op{Op: "mkdir", Path: "empty"}, write("new/n.txt", "new"), write("old.txt", "OLD"))
	if len(fails) != 0 {
		t.Fatalf("failures: %+v", fails)
	}
	if s, _ := f.read("old.txt"); s != "OLD" {
		t.Fatalf("old.txt = %q", s)
	}
	if _, ok := f.read("stale/gone.txt"); ok {
		t.Fatal("stale/gone.txt survived")
	}
	if _, err := os.Stat(filepath.Join(f.root, "stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale/ survived")
	}
	if fi, err := os.Stat(filepath.Join(f.root, "empty")); err != nil || !fi.IsDir() {
		t.Fatal("empty/ was not created")
	}
	// The manifest lives in the state directory, never in the root.
	if _, err := os.Stat(filepath.Join(f.state, "proj.json")); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(f.root, "new")); len(ents) != 1 {
		t.Fatalf("new/ holds %v: a temporary file was left", ents)
	}

	got, r := f.manifest(mirror.ManifestParams{})
	if r.Fresh || r.Hashed != 0 {
		t.Fatalf("fresh %v hashed %d: the Guest Service's own writes must be recorded", r.Fresh, r.Hashed)
	}
	if got["old.txt"].Hash != sum("OLD") || got["new/n.txt"].Hash != sum("new") || got["old.txt"].Modified {
		t.Fatalf("entries = %+v", got)
	}

	// Same size and mtime, other content: ctime (and on Windows ChangeTime) still moves.
	p := filepath.Join(f.root, "old.txt")
	fi, _ := os.Stat(p)
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(p, []byte("NEW"), 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	// A touch without a content change is rehashed but is no guest change.
	q := filepath.Join(f.root, "new", "n.txt")
	os.Chtimes(q, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	got, r = f.manifest(mirror.ManifestParams{})
	if r.Hashed != 2 {
		t.Fatalf("hashed %d, want 2 (old.txt and n.txt)", r.Hashed)
	}
	if e := got["old.txt"]; e.Hash != sum("NEW") || !e.Modified {
		t.Fatalf("old.txt = %+v, want the new hash, marked modified", e)
	}
	if got["new/n.txt"].Modified {
		t.Fatal("a touched file is not a modified one")
	}
	// Created in the guest after a sync: modified too.
	f.put("guest.txt", "g")
	got, _ = f.manifest(mirror.ManifestParams{})
	if !got["guest.txt"].Modified {
		t.Fatal("a file created in the guest is a guest change")
	}
	_, r = f.manifest(mirror.ManifestParams{Verify: true})
	if r.Hashed != 3 {
		t.Fatalf("verify hashed %d, want every file (3)", r.Hashed)
	}
}

func TestAFileStreamedOverSeveralCallsLandsWhole(t *testing.T) {
	f := newFixture(t)
	f.manifest(mirror.ManifestParams{})
	whole := strings.Repeat("0123456789", 1000)
	h := sum(whole)
	if fails := f.apply(false, mirror.Op{Op: "write", Path: "big.bin", Data: []byte(whole[:4000])}); len(fails) != 0 {
		t.Fatal(fails)
	}
	if _, ok := f.read("big.bin"); ok {
		t.Fatal("the target appeared before its last chunk")
	}
	fails := f.apply(true,
		mirror.Op{Op: "write", Path: "big.bin", Offset: 4000, Data: []byte(whole[4000:9000])},
		mirror.Op{Op: "write", Path: "big.bin", Offset: 9000, Data: []byte(whole[9000:]), End: true, Hash: h})
	if len(fails) != 0 {
		t.Fatal(fails)
	}
	if s, _ := f.read("big.bin"); s != whole {
		t.Fatalf("big.bin has %d bytes", len(s))
	}
}

func TestBadWritesFailAloneAndLeaveTheTargetAsItWas(t *testing.T) {
	f := newFixture(t)
	f.put("t.txt", "original")
	f.manifest(mirror.ManifestParams{})
	fails := f.apply(true,
		mirror.Op{Op: "write", Path: "t.txt", Data: []byte("changed"), End: true, Hash: sum("something else")},
		mirror.Op{Op: "write", Path: "gap.txt", Offset: 5, Data: []byte("x"), End: true, Hash: sum("x")},
		write("ok.txt", "ok"),
		mirror.Op{Op: "rmdir", Path: "notempty"})
	if len(fails) != 2 || fails[0].Path != "t.txt" || !strings.Contains(fails[0].Message, "changed while it was sent") || fails[1].Path != "gap.txt" {
		t.Fatalf("failures = %+v", fails)
	}
	if s, _ := f.read("t.txt"); s != "original" {
		t.Fatalf("t.txt = %q", s)
	}
	if s, _ := f.read("ok.txt"); s != "ok" {
		t.Fatal("a failure stopped the rest of the batch")
	}
	ents, _ := os.ReadDir(f.root)
	for _, e := range ents {
		if strings.Contains(e.Name(), "glov-sync") {
			t.Fatalf("temporary file %s left behind", e.Name())
		}
	}
}

// The share folder is swapped for a link while a write spans calls: the next
// call is refused, and the write's temporary file is removed from the folder
// it was written in, through the root opened before the swap.
func TestARootSwappedForALinkLeavesNoTemporaryFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a root held open between calls cannot be renamed on Windows, so it cannot be swapped")
	}
	f := newFixture(t)
	f.manifest(mirror.ManifestParams{})
	f.apply(false, mirror.Op{Op: "write", Path: "d/big.bin", Data: []byte("first chunk")})
	if len(tempFiles(t, f.root)) != 1 {
		t.Fatal("the first chunk left no temporary file")
	}
	moved := f.root + ".moved"
	if err := os.Rename(f.root, moved); err != nil {
		t.Fatal(err)
	}
	plantLink(t, moved, f.root)
	_, err := f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Ops: []mirror.Op{
		{Op: "write", Path: "d/big.bin", Offset: 11, Data: []byte("x"), End: true, Hash: sum("first chunkx")}}})
	if err == nil {
		t.Fatal("apply through a swapped root succeeded")
	}
	if left := tempFiles(t, moved); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestANonEmptyDirectoryIsKept(t *testing.T) {
	f := newFixture(t)
	f.put("d/bin/z", "excluded content")
	f.manifest(mirror.ManifestParams{Exclude: []mirror.Rule{{Pattern: `^(?:.*/)?bin$`, DirOnly: true}}})
	if fails := f.apply(true, mirror.Op{Op: "rmdir", Path: "d"}); len(fails) != 0 {
		t.Fatal(fails)
	}
	if s, _ := f.read("d/bin/z"); s != "excluded content" {
		t.Fatal("d/bin/z is gone")
	}
}

func TestPagesCoverALargeTree(t *testing.T) {
	f := newFixture(t)
	for i := range 7000 {
		f.put(fmt.Sprintf("d%02d/file-with-a-longish-name-%05d.txt", i%50, i), "x")
	}
	p := mirror.ManifestParams{ID: "proj", Root: f.root}
	r, err := f.m.Manifest(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 7050 || len(r.Entries) >= r.Total {
		t.Fatalf("first page %d of %d: a page must stay under the frame limit", len(r.Entries), r.Total)
	}
	got, _ := f.manifest(mirror.ManifestParams{})
	if len(got) != 7050 {
		t.Fatalf("%d entries over all pages, want 7050", len(got))
	}
	p.Offset = 99999
	if _, err := f.m.Manifest(p); err == nil {
		t.Fatal("an offset past the listing was accepted")
	}
}

func TestASubdirListingKeepsTheRestOfTheManifest(t *testing.T) {
	f := newFixture(t)
	f.put("a/x", "x")
	f.put("b/y", "y")
	f.manifest(mirror.ManifestParams{})
	f.apply(true)
	got, r := f.manifest(mirror.ManifestParams{Subdir: "a"})
	if !slices.Equal(keys(got), []string{"a/x"}) || r.Hashed != 0 {
		t.Fatalf("subdir listing %v, hashed %d", keys(got), r.Hashed)
	}
	f.apply(true)
	if _, r = f.manifest(mirror.ManifestParams{}); r.Hashed != 0 {
		t.Fatalf("hashed %d after a subdir sync: the records outside it were dropped", r.Hashed)
	}
}

func TestAManifestForAnotherRootIsDiscarded(t *testing.T) {
	f := newFixture(t)
	f.put("a", "a")
	f.manifest(mirror.ManifestParams{})
	f.apply(true)
	other := f.root + "2"
	os.Rename(f.root, other)
	r, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: other})
	if err != nil || !r.Fresh || r.Hashed != 1 {
		t.Fatalf("fresh %v hashed %d err %v", r.Fresh, r.Hashed, err)
	}
}

func TestInvalidParamsAreRefused(t *testing.T) {
	f := newFixture(t)
	for _, p := range []mirror.ApplyParams{
		{ID: "../x", Root: f.root},
		{ID: ".hidden", Root: f.root},
		{ID: "p", Root: "relative"},
		{ID: "p", Root: f.root, Ops: []mirror.Op{{Op: "write", Path: "../escape"}}},
		{ID: "p", Root: f.root, Ops: []mirror.Op{{Op: "write", Path: "/abs"}}},
		{ID: "p", Root: f.root, Ops: []mirror.Op{{Op: "write", Path: "a:stream"}}},
		{ID: "p", Root: f.root, Ops: []mirror.Op{{Op: "write", Path: `a\b`}}},
		{ID: "p", Root: f.root, Ops: []mirror.Op{{Op: "delete", Path: ""}}},
		{ID: "p", Root: f.root, Ops: []mirror.Op{{Op: "chmod", Path: "a"}}},
	} {
		_, err := f.m.Apply(p)
		if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeInvalidParams {
			t.Errorf("%+v: err = %v, want invalid params", p, err)
		}
	}
	if _, err := f.m.Manifest(mirror.ManifestParams{ID: "p", Root: f.root, Exclude: []mirror.Rule{{Pattern: "("}}}); err == nil {
		t.Error("a bad exclude pattern was accepted")
	}
}

// outside is a directory beside the root with one file, and a check that
// nothing in it changed.
func outside(t *testing.T, f *fixture) (string, func()) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(f.root), "outside")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "victim.txt"), []byte("victim"), 0o644)
	return dir, func() {
		t.Helper()
		ents, _ := os.ReadDir(dir)
		b, _ := os.ReadFile(filepath.Join(dir, "victim.txt"))
		if len(ents) != 1 || string(b) != "victim" {
			t.Fatalf("the directory outside the root was changed: %v, victim.txt = %q", ents, b)
		}
	}
}

func TestALinkPlantedInTheRootIsNeverFollowed(t *testing.T) {
	f := newFixture(t)
	f.put("keep.txt", "k")
	out, unchanged := outside(t, f)
	plantLink(t, out, filepath.Join(f.root, "esc"))
	got, _ := f.manifest(mirror.ManifestParams{})
	if _, ok := got["esc"]; ok {
		t.Fatal("the link was listed")
	}
	fails := f.apply(true,
		mirror.Op{Op: "delete", Path: "esc/victim.txt"},
		mirror.Op{Op: "mkdir", Path: "esc/sub"},
		write("esc/victim.txt", "pwned"),
		write("esc/new.txt", "pwned"),
		mirror.Op{Op: "write", Path: "esc/big.bin", Data: []byte("pw")},
		mirror.Op{Op: "write", Path: "esc/big.bin", Offset: 2, Data: []byte("ned"), End: true, Hash: sum("pwned")})
	if len(fails) != 5 {
		t.Fatalf("failures = %+v, want all five operations through the link refused", fails)
	}
	unchanged()
}

func TestADirectorySwappedForALinkAfterTheListingIsRefused(t *testing.T) {
	f := newFixture(t)
	f.put("d/f.txt", "f")
	out, unchanged := outside(t, f)
	f.manifest(mirror.ManifestParams{})
	// Between the listing and the apply, a guest process swaps d for a link.
	if err := os.Rename(filepath.Join(f.root, "d"), filepath.Join(f.root, "d-old")); err != nil {
		t.Fatal(err)
	}
	plantLink(t, out, filepath.Join(f.root, "d"))
	fails := f.apply(true,
		mirror.Op{Op: "delete", Path: "d/victim.txt"},
		write("d/victim.txt", "pwned"),
		write("d/x.txt", "pwned"),
		mirror.Op{Op: "mkdir", Path: "d/sub"})
	if len(fails) != 4 {
		t.Fatalf("failures = %+v, want all four refused", fails)
	}
	unchanged()
}

// refusedThroughLink checks that neither listing nor changing the share at
// f.root works, and that the call says why.
func refusedThroughLink(t *testing.T, f *fixture) {
	t.Helper()
	r, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root})
	if err == nil || !strings.Contains(err.Error(), "link or junction") || len(r.Entries) != 0 {
		t.Fatalf("manifest = %+v, %v; want refused", r, err)
	}
	_, err = f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Commit: true, Ops: []mirror.Op{
		{Op: "delete", Path: "victim.txt"}, {Op: "mkdir", Path: "sub"}, write("victim.txt", "pwned"), write("new.txt", "pwned")}})
	if err == nil || !strings.Contains(err.Error(), "link or junction") {
		t.Fatalf("apply = %v; want refused", err)
	}
}

// A guest process that may change C:\work replaces the share folder with a
// junction elsewhere: the Guest Service (LocalSystem) must not list, write or delete
// there.
func TestARootThatIsALinkIsRefused(t *testing.T) {
	f := newFixture(t)
	out, unchanged := outside(t, f)
	plantLink(t, out, f.root)
	refusedThroughLink(t, f)
	unchanged()
}

func TestARootBelowALinkIsRefused(t *testing.T) {
	f := newFixture(t)
	out, unchanged := outside(t, f)
	plantLink(t, filepath.Dir(out), filepath.Join(filepath.Dir(f.root), "via"))
	f.root = filepath.Join(filepath.Dir(f.root), "via", "outside")
	refusedThroughLink(t, f)
	f.root = filepath.Join(filepath.Dir(f.root), "missing", "root") // would be created through the link
	refusedThroughLink(t, f)
	unchanged()
	if _, err := os.Stat(filepath.Join(filepath.Dir(out), "missing")); err == nil {
		t.Fatal("a folder was created through the link")
	}
}

// A root replaced by a link between two apply calls is caught by the second.
func TestARootSwappedForALinkBetweenCallsIsRefused(t *testing.T) {
	f := newFixture(t)
	f.manifest(mirror.ManifestParams{})
	f.apply(false, mirror.Op{Op: "write", Path: "big.bin", Data: []byte("pw")})
	out, unchanged := outside(t, f)
	if err := os.Rename(f.root, f.root+"-old"); err != nil {
		t.Skipf("the root is held open during a streamed write: %v", err)
	}
	plantLink(t, out, f.root)
	_, err := f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Commit: true, Ops: []mirror.Op{
		{Op: "write", Path: "big.bin", Offset: 2, Data: []byte("ned"), End: true, Hash: sum("pwned")}, {Op: "delete", Path: "victim.txt"}}})
	if err == nil || !strings.Contains(err.Error(), "link or junction") {
		t.Fatalf("apply = %v; want refused", err)
	}
	unchanged()
}

// After a sync the Guest Service holds nothing open in the share: on Windows an open
// handle would stop the user renaming or deleting the folder.
func TestASyncLeavesTheShareFolderFree(t *testing.T) {
	f := newFixture(t)
	f.manifest(mirror.ManifestParams{})
	f.apply(true, write("a.txt", "a"))
	if err := os.Rename(f.root, f.root+"-renamed"); err != nil {
		t.Fatalf("the share folder is still held: %v", err)
	}
}

// A file the Guest Service may not write is reported as denied, not as held by
// another process; a refused delete or replace may be either.
func TestAccessDeniedIsNotReportedAsLocked(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test cannot write: Unix, not root")
	}
	f := newFixture(t)
	f.put("ro/old.txt", "o")
	f.manifest(mirror.ManifestParams{})
	ro := filepath.Join(f.root, "ro")
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	fails := f.apply(true, write("ro/new.txt", "n"), mirror.Op{Op: "delete", Path: "ro/old.txt"})
	if len(fails) != 2 || fails[0].Path != "ro/new.txt" || !fails[0].Denied || fails[0].Locked ||
		fails[1].Path != "ro/old.txt" || !fails[1].Denied || !fails[1].Locked {
		t.Fatalf("failures = %+v", fails)
	}
}
