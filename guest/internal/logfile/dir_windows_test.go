package logfile_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/akunzai/glov/guest/internal/logfile"
)

func TestNewDirectoryIsCreatedAndAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "glov", "guest-service.log")
	w, err := logfile.Open(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("x"))
	w.Close()
	// Opening again takes the "already exists" path, which re-verifies the DACL it just set.
	w, err = logfile.Open(path, 1000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	w.Close()
}

// A directory made the ordinary way inherits an ACL that lets the creating
// user (and, under C:\, Authenticated Users) write: exactly what a planted
// C:\ProgramData\glov looks like.
func TestPreExistingUserDirectoryIsRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "planted")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make it what a planted folder is: writable by ordinary users, whatever the parent's ACL.
	if out, err := exec.Command("icacls.exe", dir, "/grant", "*S-1-5-32-545:(OI)(CI)M").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	w, err := logfile.Open(filepath.Join(dir, "guest-service.log"), 1000)
	if err == nil {
		w.Close()
		t.Fatal("accepted a directory writable by others")
	}
	t.Log("rejected:", err)
	if _, serr := os.Stat(filepath.Join(dir, "guest-service.log")); serr == nil {
		t.Fatal("log file was created in the rejected directory")
	}
}

func TestJunctionIsRejected(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	os.Mkdir(real, 0o755)
	junction := filepath.Join(base, "junction")
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, real).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v %s", err, out)
	}
	if w, err := logfile.Open(filepath.Join(junction, "guest-service.log"), 1000); err == nil {
		w.Close()
		t.Fatal("followed a junction")
	} else {
		t.Log("rejected:", err)
	}
	if entries, _ := os.ReadDir(real); len(entries) != 0 {
		t.Fatalf("wrote through the junction: %v", entries)
	}
}

func TestJunctionAsLogFileIsRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "glov")
	w, err := logfile.Open(filepath.Join(dir, "guest-service.log"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Remove(filepath.Join(dir, "guest-service.log"))
	target := t.TempDir()
	if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", filepath.Join(dir, "guest-service.log"), target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v %s", err, out)
	}
	if w, err := logfile.Open(filepath.Join(dir, "guest-service.log"), 1000); err == nil {
		w.Close()
		t.Fatal("opened a junction as the log file")
	}
}
