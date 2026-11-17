package hostclock

import (
	"errors"
	"testing"
	"time"
)

type fake struct {
	host, system time.Time
	hostErr      error
	setErr       error
	set          []time.Time
	reads        int
}

func (f *fake) clocks() Clocks {
	return Clocks{
		Host: func() (time.Time, error) {
			f.reads++
			return f.host, f.hostErr
		},
		System: func() time.Time { return f.system },
		Set: func(t time.Time) error {
			if f.setErr != nil {
				return f.setErr
			}
			f.set = append(f.set, t)
			return nil
		},
	}
}

var base = time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)

func TestCheckLeavesAClockWithinTheThresholdAlone(t *testing.T) {
	for _, off := range []time.Duration{0, Threshold, -Threshold, 300 * time.Millisecond} {
		f := &fake{host: base, system: base.Add(-off)}
		step, err := Check(f.clocks())
		if err != nil || step != 0 || len(f.set) != 0 {
			t.Errorf("offset %v: step %v, err %v, set %v", off, step, err, f.set)
		}
	}
}

func TestCheckStepsAClockLeftBehindToTheHosts(t *testing.T) {
	// A host's Modern Standby stops the guest clock: it wakes 128 s behind.
	f := &fake{host: base, system: base.Add(-128 * time.Second)}
	step, err := Check(f.clocks())
	if err != nil || step != 128*time.Second {
		t.Fatalf("step %v, err %v", step, err)
	}
	if len(f.set) != 1 || !f.set[0].Equal(base) || f.reads != 2 {
		t.Errorf("set %v after %d host reads", f.set, f.reads)
	}
}

func TestCheckStepsAClockAheadBack(t *testing.T) {
	f := &fake{host: base, system: base.Add(5 * time.Second)}
	if step, err := Check(f.clocks()); err != nil || step != -5*time.Second || len(f.set) != 1 {
		t.Errorf("step %v, err %v, set %v", step, err, f.set)
	}
}

func TestCheckReportsErrorsWithoutStepping(t *testing.T) {
	boom := errors.New("boom")
	f := &fake{host: base, system: base.Add(-time.Minute), hostErr: boom}
	if _, err := Check(f.clocks()); !errors.Is(err, boom) || len(f.set) != 0 {
		t.Errorf("host error: err %v, set %v", err, f.set)
	}
	f = &fake{host: base, system: base.Add(-time.Minute), setErr: boom}
	if step, err := Check(f.clocks()); !errors.Is(err, boom) || step != 0 {
		t.Errorf("set error: step %v, err %v", step, err)
	}
}
