package logfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akunzai/glov/guest/internal/logfile"
)

func TestRotatesAtCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "guest-service.log")
	w, err := logfile.Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := strings.Repeat("a", 40) + "\n"
	for range 10 {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := os.ReadFile(path)
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal("no rotated file:", err)
	}
	if len(cur) > 100 || len(old) > 100 || len(cur) == 0 {
		t.Fatalf("sizes cur=%d old=%d, want both within the 100 byte cap", len(cur), len(old))
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 2 {
		t.Fatalf("%d files kept, want 2", len(entries))
	}
}

func TestRefusesSymlinkedLogFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("keep"), 0o644)
	path := filepath.Join(dir, "guest-service.log")
	if err := os.Symlink(target, path); err != nil {
		t.Skip("cannot create symlinks:", err)
	}
	if w, err := logfile.Open(path, 100); err == nil {
		w.Close()
		t.Fatal("opened a symlinked log file")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target was written: %q", b)
	}
}

func TestRefusesSymlinkedLogDirectory(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	os.Mkdir(real, 0o755)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("cannot create symlinks:", err)
	}
	if w, err := logfile.Open(filepath.Join(link, "guest-service.log"), 100); err == nil {
		w.Close()
		t.Fatal("opened a log inside a symlinked directory")
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
}
