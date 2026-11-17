package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/secret"
)

func TestRecordsUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "claude", "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{"claude/a.jsonl": 100, "claude/p/b.jsonl": 24} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A link out of the folder is not followed (it may need privileges to make).
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "big"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Symlink(outside, filepath.Join(dir, "claude", "link")) == nil {
		_ = os.Symlink(filepath.Join(outside, "big"), filepath.Join(dir, "filelink"))
	}
	bytes, files, ok := recordsUsage(dir)
	if !ok || bytes != 124 || files != 2 {
		t.Fatalf("usage = %d bytes, %d files, %v; want 124, 2, true", bytes, files, ok)
	}
	if _, _, ok := recordsUsage(filepath.Join(dir, "missing")); ok {
		t.Fatal("a missing folder reported as existing")
	}
}

func TestDescribeRecords(t *testing.T) {
	if got, want := describeRecords(`C:\r\vm`, 124, 2, true), `  conversation records: C:\r\vm (2 files, 1 KiB)`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := describeRecords(`C:\r\vm`, 0, 0, false), `  conversation records: C:\r\vm (none yet)`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := describeRecords("d", 3<<20, 9, true); !strings.HasSuffix(got, "(9 files, 3.0 MiB)") {
		t.Errorf("got %q", got)
	}
}

func TestKeptRecordsLine(t *testing.T) {
	want := `kept its conversation records at C:\r\vm; remove them yourself when no longer needed`
	if got := keptRecordsLine(`C:\r\vm`); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCommandSourcesIn(t *testing.T) {
	list := []api.Secret{
		{Name: "gh", Source: secret.Command},
		{Name: "key", Source: secret.Stored},
		{Name: "other", Source: secret.Command},
	}
	got := commandSourcesIn(list, []string{"gh", "key"})
	if len(got) != 1 || got[0].Name != "gh" {
		t.Fatalf("got %v, want only gh", got)
	}
	if len(commandSourcesIn(list, nil)) != 0 {
		t.Fatal("no names selected something")
	}
	if len(list) != 3 {
		t.Fatal("the list was modified")
	}
}

func TestSourcesToRefresh(t *testing.T) {
	list := []api.Secret{
		{Name: "gh", Source: secret.Command},
		{Name: "key", Source: secret.Stored},
		{Name: "glab", Source: secret.Command},
		{Name: "ungranted", Source: secret.Command},
	}
	got := sourcesToRefresh(list, []string{"gh", "key", "glab", "gh", "gone"}, map[string]bool{"glab": true})
	if len(got) != 1 || got[0].Name != "gh" {
		t.Fatalf("got %v, want only gh: stored, ungranted and already refreshed ones stay out", got)
	}
}
