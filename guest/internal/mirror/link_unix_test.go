//go:build unix

package mirror_test

import (
	"os"
	"testing"
)

// plantLink makes at a symbolic link to target, as a guest process could.
func plantLink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}
