//go:build !windows && !linux

package guestservice

import (
	"errors"
	"os"
	"strings"
	"time"
)

// These stubs keep the package building and testable on other platforms; the
// real implementations are platform_windows.go and platform_linux.go.

func hostInfo() (hostname string, user *string, bootID string) {
	hostname, _ = os.Hostname()
	bootID = "unknown"
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = strings.TrimSpace(string(b))
	}
	return hostname, nil, bootID
}

func setSystemTime(time.Time) error { return errors.ErrUnsupported }

// prepareReboot is nil: power.reboot is not served.
var prepareReboot func() (func() error, error)
