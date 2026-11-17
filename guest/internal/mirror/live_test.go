package mirror_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/akunzai/glov/guest/internal/mirror"
	"github.com/akunzai/glov/guest/proto"
)

func ptr(s string) *string { return &s }

func TestAManifestOfPathsListsJustThose(t *testing.T) {
	f := newFixture(t)
	f.put("a.txt", "a")
	f.put("d/b.txt", "b")
	f.put("d/c.txt", "c")
	f.put("out/x", "x")
	plantLink(t, filepath.Join(f.root, "d"), filepath.Join(f.root, "link"))
	r, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root,
		Exclude: []mirror.Rule{{Pattern: `^out$`, DirOnly: true}},
		Paths:   []string{"a.txt", "d", "d/b.txt", "gone", "gone/deeper", "link", "link/b.txt", "out/x"}})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, e := range r.Entries {
		listed = append(listed, e.Path)
		if e.Path == "a.txt" && e.Hash != sum("a") || e.Path == "d" && !e.Dir {
			t.Errorf("entry %+v", e)
		}
		if e.Path == "a.txt" {
			fi, _ := os.Stat(filepath.Join(f.root, "a.txt"))
			if e.Mtime != fi.ModTime().UnixNano() {
				t.Errorf("a.txt mtime %d, want %d", e.Mtime, fi.ModTime().UnixNano())
			}
		}
	}
	if want := []string{"a.txt", "d", "d/b.txt"}; !slices.Equal(listed, want) {
		t.Errorf("listed %v, want %v", listed, want)
	}
	// A link, and a path through one, are what a listing does not reach.
	if want := []string{"gone", "gone/deeper", "link", "link/b.txt"}; !slices.Equal(r.Missing, want) {
		t.Errorf("missing %v, want %v (out/x is excluded: neither)", r.Missing, want)
	}
}

func TestAManifestOfPathsRefusesBadOnes(t *testing.T) {
	f := newFixture(t)
	for _, paths := range [][]string{{""}, {"../x"}, {"a\\b"}, make([]string, 513)} {
		if len(paths) == 513 {
			for i := range paths {
				paths[i] = "f"
			}
		}
		_, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root, Paths: paths})
		if pe, ok := err.(*proto.Error); !ok || pe.Code != proto.CodeInvalidParams {
			t.Errorf("paths %q: %v, want invalid params", paths[0], err)
		}
	}
}

func TestExpectKeepsAGuestChangeTheHostHasNotSeen(t *testing.T) {
	f := newFixture(t)
	f.put("a.txt", "guest edit")
	w := write("a.txt", "host")
	w.Expect = ptr(sum("old"))
	d := mirror.Op{Op: "delete", Path: "a.txt", Expect: ptr(sum("old"))}
	n := write("new.txt", "host")
	n.Expect = ptr("")
	failures := f.apply(true, w, d, n)
	if len(failures) != 2 || !failures[0].Conflict || !failures[1].Conflict {
		t.Fatalf("failures %+v, want two conflicts", failures)
	}
	if got, _ := f.read("a.txt"); got != "guest edit" {
		t.Errorf("a.txt = %q, want the guest's edit kept", got)
	}
	if got, _ := f.read("new.txt"); got != "host" {
		t.Errorf("new.txt = %q, want it written (expected absent)", got)
	}
	// What the guest holds now is expected: the write goes through.
	w.Expect = ptr(sum("guest edit"))
	if failures := f.apply(true, w); len(failures) != 0 {
		t.Fatalf("failures %+v", failures)
	}
	if got, _ := f.read("a.txt"); got != "host" {
		t.Errorf("a.txt = %q, want the host's", got)
	}
	// A file where none was expected is a conflict too.
	n.Data, n.Hash = []byte("again"), sum("again")
	if failures := f.apply(true, n); len(failures) != 1 || !failures[0].Conflict {
		t.Errorf("failures %+v, want a conflict", failures)
	}
	d.Expect = ptr(sum("host"))
	if failures := f.apply(true, d); len(failures) != 0 {
		t.Fatalf("failures %+v", failures)
	}
	if _, ok := f.read("a.txt"); ok {
		t.Error("a.txt still there after an expected delete")
	}
}

func TestAWriteKeepsTheModeOfTheFileItReplaces(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("NTFS has no permission bits")
	}
	f := newFixture(t)
	f.put("run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(f.root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if failures := f.apply(true, write("run.sh", "#!/bin/sh\necho hi\n"), write("new.sh", "x")); len(failures) != 0 {
		t.Fatal(failures)
	}
	for name, want := range map[string]os.FileMode{"run.sh": 0o755, "new.sh": 0o644} {
		fi, err := os.Stat(filepath.Join(f.root, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode %o, want %o", name, got, want)
		}
	}
}
