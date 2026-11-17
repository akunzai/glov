package mirror_test

import (
	"os/exec"
	"testing"
)

// plantLink makes at a junction to target, as any guest process can without
// privileges.
func plantLink(t *testing.T, target, at string) {
	t.Helper()
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", at, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}
