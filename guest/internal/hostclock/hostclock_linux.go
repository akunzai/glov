package hostclock

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Watch keeps the system clock on the host's until ctx ends. Without Device
// (not Hyper-V, or hv_utils not loaded) it returns at once.
func Watch(ctx context.Context) {
	f, err := os.Open(Device)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("host clock unavailable", "device", Device, "err", err)
		}
		return
	}
	defer f.Close()
	c := Clocks{
		Host:   func() (time.Time, error) { return phcTime(f) },
		System: time.Now,
		Set: func(t time.Time) error {
			ts := unix.NsecToTimespec(t.UnixNano())
			return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
		},
	}
	slog.Info("following the host clock", "device", Device)
	var lastErr string
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		switch step, err := Check(c); {
		case err != nil:
			if msg := err.Error(); msg != lastErr { // one line per distinct failure
				slog.Warn("host clock check failed", "err", msg)
				lastErr = msg
			}
		case step != 0:
			lastErr = ""
			slog.Info("stepped the clock to the host's", "step", step.Round(time.Millisecond).String())
		default:
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// phcTime reads a PTP clock through its dynamic POSIX clock ID (the
// kernel's FD_TO_CLOCKID).
func phcTime(f *os.File) (time.Time, error) {
	var ts unix.Timespec
	id := int32((^int(f.Fd()))<<3 | 3)
	if err := unix.ClockGettime(id, &ts); err != nil {
		return time.Time{}, err
	}
	return time.Unix(ts.Unix()), nil
}
