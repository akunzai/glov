package guestservice

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHostInfoOnLinux(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "1"), []byte("UID=1000\nUSER=alice\nSTATE=active\nTYPE=wayland\nCLASS=user\nSEAT=seat0\n"), 0o644)
	old := sessionsDir
	sessionsDir = dir
	defer func() { sessionsDir = old }()

	host, user, boot := hostInfo()
	if host == "" {
		t.Error("empty hostname")
	}
	if user == nil || *user != "alice" {
		t.Errorf("user = %v, want alice", user)
	}
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err != nil || strings.TrimSpace(string(b)) != boot {
		t.Errorf("bootId = %q, want the kernel boot_id", boot)
	}

	sessionsDir = filepath.Join(dir, "none")
	if _, user, _ := hostInfo(); user != nil {
		t.Errorf("user = %v with nobody signed in, want null", *user)
	}
}

func TestSetSystemTimePassesTheHostClock(t *testing.T) {
	var got unix.Timespec
	old := clockSet
	clockSet = func(ts *unix.Timespec) error { got = *ts; return nil }
	defer func() { clockSet = old }()

	if err := setSystemTime(time.UnixMilli(1700000000123)); err != nil {
		t.Fatal(err)
	}
	if got.Sec != 1700000000 || got.Nsec != 123_000_000 {
		t.Fatalf("clock_settime(%d.%09d)", got.Sec, got.Nsec)
	}
	clockSet = func(*unix.Timespec) error { return unix.EPERM }
	if err := setSystemTime(time.Now()); !errors.Is(err, unix.EPERM) {
		t.Fatalf("error not propagated: %v", err)
	}
}
