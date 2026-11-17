// Package hostclock keeps a Linux guest's clock on the host's under Hyper-V.
//
// A guest's clock stands still while a Windows host is in Modern Standby, and
// nothing steps it when the host wakes: Hyper-V's time synchronization sends
// a sync sample only when the host's own time changes, and an isolated VM has
// no NTP source. The hyperv PTP clock (/dev/ptp_hyperv, from
// hv_utils' TimeSync) follows the host's samples, which arrive every few
// seconds, so the Guest Service compares it with the system clock and steps
// the system clock when they part by more than Threshold.
package hostclock

import "time"

const (
	// Device is the PTP clock hv_utils exposes for Hyper-V's time samples.
	Device = "/dev/ptp_hyperv"
	// Threshold is how far the system clock may drift from the host's
	// before it is stepped. It is well above what an NTP daemon leaves, so
	// the two never fight.
	Threshold = time.Second
	// Interval is how often the clocks are compared.
	Interval = 2 * time.Second
)

// Clocks are the clocks Check reads and sets.
type Clocks struct {
	// Host reads the host's time.
	Host func() (time.Time, error)
	// System reads the system clock.
	System func() time.Time
	// Set steps the system clock.
	Set func(time.Time) error
}

// Check steps the system clock to the host's when they differ by more than
// Threshold, and returns the step (zero when none was needed): positive
// when the system clock was behind.
func Check(c Clocks) (time.Duration, error) {
	host, err := c.Host()
	if err != nil {
		return 0, err
	}
	off := host.Sub(c.System())
	if off.Abs() <= Threshold {
		return 0, nil
	}
	// Re-read the host right before setting, so the time spent reading the
	// system clock does not end up in the step.
	if host, err = c.Host(); err != nil {
		return 0, err
	}
	if err := c.Set(host); err != nil {
		return 0, err
	}
	return off, nil
}
