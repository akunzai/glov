//go:build unix

package daemon

import (
	"os"
	"testing"
	"time"
)

// writeBootLog replaces the console log at path with a new file, which
// gets a new inode: it is written beside the old one, then renamed over it,
// since a file system such as ext4 may hand a removed file's inode to the
// next file created.
func writeBootLog(t *testing.T, path, data string, _ time.Time) {
	t.Helper()
	if err := os.WriteFile(path+".new", []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}
