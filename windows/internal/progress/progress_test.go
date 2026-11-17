package progress

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type lines struct {
	mu sync.Mutex
	l  []string
}

func (l *lines) add(s string) { l.mu.Lock(); l.l = append(l.l, s); l.mu.Unlock() }
func (l *lines) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.l...)
}

func TestWhileRunningSaysStillUntilDone(t *testing.T) {
	var got lines
	err := WhileRunning("converting x", got.add, 20*time.Millisecond, func() error {
		time.Sleep(110 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	l := got.get()
	if len(l) < 3 || l[0] != "converting x" {
		t.Fatalf("lines = %q, want the step then still lines", l)
	}
	for _, s := range l[1:] {
		if !strings.HasPrefix(s, "still converting x (0m 0") || !strings.HasSuffix(s, "s)") {
			t.Errorf("line %q is not a still line", s)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if after := got.get(); len(after) != len(l) {
		t.Errorf("still lines after the step ended: %q", after[len(l):])
	}
}

func TestStepRestartsTheClock(t *testing.T) {
	var got lines
	h := Start("booting", got.add, 40*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	h.Step("installing")
	time.Sleep(25 * time.Millisecond)
	if l := got.get(); len(l) != 2 || l[1] != "installing" {
		t.Errorf("lines = %q, want no still line before the interval passes the new step", l)
	}
	time.Sleep(40 * time.Millisecond)
	h.Stop()
	h.Step("ignored")
	l := got.get()
	if len(l) < 3 || !strings.HasPrefix(l[2], "still installing ") || l[len(l)-1] == "ignored" {
		t.Errorf("lines = %q, want a still line naming the newest step and nothing after Stop", l)
	}
}

func TestPollReportsChangesAndStillLines(t *testing.T) {
	var got lines
	var mu sync.Mutex
	phase := "dev: running, Guest Service connecting"
	stop := Poll(func() string { mu.Lock(); defer mu.Unlock(); return phase }, got.add, 5*time.Millisecond, 100*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	phase = "dev: running, Guest Service ready"
	mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	stop()
	n := len(got.get())
	time.Sleep(20 * time.Millisecond)
	l := got.get()
	if len(l) != n {
		t.Fatalf("reported after stop: %q", l[n:])
	}
	if len(l) != 3 || !strings.HasPrefix(l[0], "dev: running, Guest Service connecting (") ||
		!strings.HasPrefix(l[1], "still dev: running, Guest Service connecting (") ||
		!strings.HasPrefix(l[2], "dev: running, Guest Service ready (") {
		t.Errorf("lines = %q, want the phase, a still line, then the new phase", l)
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 6 * time.Minute: "6m", 365 * time.Second: "6m 05s"} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}
