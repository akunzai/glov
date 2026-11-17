package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// A junction already in the host tree is not written through: it could lead
// out of the folder the copy allowlist let in.
func TestCopyFromTheGuestSkipsJunctionsOnTheHost(t *testing.T) {
	g := newMemGuest()
	g.mkdirAll("/root/proj/out")
	g.files["/root/proj/out/x"] = []byte("x")
	g.files["/root/proj/ok"] = []byte("ok")
	dst, outside := t.TempDir(), t.TempDir()
	junction := filepath.Join(dst, "out")
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", junction, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, b)
	}
	c, err := fromGuest(g, "/root/proj", dst)
	if err != nil || c.files != 1 || !slices.Equal(c.skipped, []string{junction}) {
		t.Fatalf("fromGuest = %+v, %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Error("wrote through the junction")
	}
}
