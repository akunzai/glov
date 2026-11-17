package daemon

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// writeBootLog writes a boot's console log at path, created at created: a
// new boot's log may reuse the file, and NTFS keeps a recreated file's
// creation time, so only the time tells boots apart.
func writeBootLog(t *testing.T, path, data string, created time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := windows.Open(path, windows.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	ft := windows.NsecToFiletime(created.UnixNano())
	if err := windows.SetFileTime(h, &ft, nil, nil); err != nil {
		t.Fatal(err)
	}
}
