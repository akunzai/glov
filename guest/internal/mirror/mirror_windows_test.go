package mirror_test

import (
	"path/filepath"
	"testing"

	"github.com/akunzai/glov/guest/internal/mirror"
	"golang.org/x/sys/windows"
)

// A file another process holds without sharing cannot be replaced or deleted:
// the failure says so, and the rest of the batch still lands.
func TestALockedFileFailsAndSaysItIsLocked(t *testing.T) {
	f := newFixture(t)
	f.put("held.txt", "held")
	f.put("held2.txt", "held2")
	f.manifest(mirror.ManifestParams{})
	for _, name := range []string{"held.txt", "held2.txt"} {
		p, _ := windows.UTF16PtrFromString(filepath.Join(f.root, name))
		h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(h)
	}
	fails := f.apply(true, mirror.Op{Op: "delete", Path: "held2.txt"}, write("held.txt", "new"), write("free.txt", "free"))
	if len(fails) != 2 || !fails[0].Locked || !fails[1].Locked || fails[0].Path != "held2.txt" || fails[1].Path != "held.txt" {
		t.Fatalf("failures = %+v", fails)
	}
	// A read-only file is not held by anyone: writing beside it is fine, and
	// only replacing or deleting it is refused.
	f.put("ro/old.txt", "o")
	f.manifest(mirror.ManifestParams{})
	ro, _ := windows.UTF16PtrFromString(filepath.Join(f.root, "ro", "old.txt"))
	if err := windows.SetFileAttributes(ro, windows.FILE_ATTRIBUTE_READONLY); err != nil {
		t.Fatal(err)
	}
	defer windows.SetFileAttributes(ro, windows.FILE_ATTRIBUTE_NORMAL)
	fails = f.apply(true, write("ro/old.txt", "new"))
	if len(fails) != 1 || !fails[0].Denied {
		t.Fatalf("failures = %+v, want access denied", fails)
	}
	if s, _ := f.read("free.txt"); s != "free" {
		t.Fatal("free.txt was not written")
	}
}
