package guestservice

import (
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/akunzai/glov/guest/internal/exec"
	"github.com/akunzai/glov/guest/internal/session"
)

// Seams for tests; the defaults act on the real machine.
var (
	sessionsDir = exec.SessionsDir
	clockSet    = func(ts *unix.Timespec) error { return unix.ClockSettime(unix.CLOCK_REALTIME, ts) }
)

func hostInfo() (hostname string, user *string, bootID string) {
	hostname, _ = os.Hostname()
	if s, ok := session.Active(sessionsDir); ok {
		user = &s.User
	}
	// boot_id is a random UUID per boot, unaffected by clock steps.
	bootID = "unknown"
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = strings.TrimSpace(string(b))
	}
	return hostname, user, bootID
}

func setSystemTime(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return clockSet(&ts)
}

// prepareReboot is nil: power.reboot is not served, because the host
// reboots a Linux guest by stopping and starting it.
var prepareReboot func() (func() error, error)
