//go:build unix

package egress

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAMarkerNotOwnedByRootDoesNotEnableTheProxy(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root, the test's marker is root-owned")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "egress-proxy")
	if err := os.WriteFile(marker, []byte(Identity), 0o644); err != nil {
		t.Fatal(err)
	}
	if enabledAt([]string{dir}, marker) {
		t.Fatal("a user-owned marker enabled the proxy")
	}
}
