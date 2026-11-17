package share

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func sha(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:])
}

type scanFixture struct {
	t     *testing.T
	dir   string
	root  *os.Root
	cache string
}

func newScanFixture(t *testing.T, files map[string]string) *scanFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "share")
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(dir, 0o755)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return &scanFixture{t, dir, root, filepath.Join(t.TempDir(), "cache", "proj.json")}
}

func (f *scanFixture) scan(scope string, verify bool, exclude ...string) Tree {
	f.t.Helper()
	ex, err := CompileExcludes(exclude)
	if err != nil {
		f.t.Fatal(err)
	}
	tree, err := Scan(f.root, scope, ex, f.cache, verify, func() bool { return false })
	if err != nil {
		f.t.Fatal(err)
	}
	return tree
}

func TestScanListsFilesAndDirsAndSkipsExcludedPathsAndJunctions(t *testing.T) {
	f := newScanFixture(t, map[string]string{"a.txt": "a", "src/b.cs": "bb", "bin/x.dll": "x", "src/obj/y": "y"})
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(f.dir, "escape"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, b)
	}
	tree := f.scan("", false, "bin/", "obj/")
	want := map[string]HostFile{"a.txt": {1, sha("a")}, "src/b.cs": {2, sha("bb")}}
	if !maps.Equal(tree.Files, want) || !maps.Equal(tree.Dirs, set("src")) || !slices.Equal(tree.Skipped, []string{"escape"}) || tree.Hashed != 2 {
		t.Fatalf("scan = %+v", tree)
	}
	if err := ScopeDir(f.root, "escape"); err == nil {
		t.Error("ScopeDir accepted a junction")
	}
	if err := ScopeDir(f.root, "missing"); err == nil {
		t.Error("ScopeDir accepted a missing directory")
	}
	if err := ScopeDir(f.root, "src"); err != nil {
		t.Error(err)
	}
	if _, err := OpenFile(f.root, "escape/secret"); err == nil {
		t.Error("OpenFile went through a junction")
	}
}

func TestScanRehashesOnlyChangedFiles(t *testing.T) {
	f := newScanFixture(t, map[string]string{"keep.txt": "k", "edit.txt": "e"})
	if n := f.scan("", false).Hashed; n != 2 {
		t.Fatalf("first scan hashed %d", n)
	}
	if n := f.scan("", false).Hashed; n != 0 {
		t.Fatalf("unchanged scan hashed %d", n)
	}
	// Same size, write time set back: the change time still moves.
	p := filepath.Join(f.dir, "edit.txt")
	fi, _ := os.Stat(p)
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(p, []byte("E"), 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	tree := f.scan("", false)
	if tree.Hashed != 1 || tree.Files["edit.txt"].Hash != sha("E") {
		t.Fatalf("scan after an edit = %+v", tree)
	}
	if n := f.scan("", true).Hashed; n != 2 {
		t.Errorf("verify hashed %d, want 2", n)
	}
}

func TestAScopedScanKeepsTheCacheOutsideIt(t *testing.T) {
	f := newScanFixture(t, map[string]string{"a/x": "x", "b/y": "y"})
	f.scan("", false)
	tree := f.scan("a", false)
	if !slices.Equal(slices.Sorted(maps.Keys(tree.Files)), []string{"a/x"}) || len(tree.Dirs) != 0 || tree.Hashed != 0 {
		t.Fatalf("scoped scan = %+v", tree)
	}
	if n := f.scan("", false).Hashed; n != 0 {
		t.Errorf("full scan after a scoped one hashed %d", n)
	}
}

func TestACancelledScanSavesNothing(t *testing.T) {
	f := newScanFixture(t, map[string]string{"a": "a", "b": "b"})
	_, err := Scan(f.root, "", Excludes{}, f.cache, false, func() bool { return true })
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Scan = %v", err)
	}
	if _, err := os.Stat(f.cache); !os.IsNotExist(err) {
		t.Errorf("cache written: %v", err)
	}
}
