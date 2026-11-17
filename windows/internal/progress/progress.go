// Package progress says that a long step with no output of its own still runs.
package progress

import (
	"fmt"
	"sync"
	"time"
)

// Every is how often a quiet step says it still runs.
const Every = 30 * time.Second

// Heartbeat reports each step, then "still <step> (<elapsed>)" every interval
// until the next step or Stop. It is safe for concurrent use, and report is
// never called concurrently or after Stop returns.
type Heartbeat struct {
	mu      sync.Mutex
	report  func(string)
	every   time.Duration
	step    string
	since   time.Time
	timer   *time.Timer
	stopped bool
}

// Start reports step and returns its Heartbeat.
func Start(step string, report func(string), every time.Duration) *Heartbeat {
	h := &Heartbeat{report: report, every: every}
	h.Step(step)
	return h
}

// Step reports step and restarts the clock.
func (h *Heartbeat) Step(step string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.step, h.since = step, time.Now()
	h.report(step)
	if h.timer == nil {
		h.timer = time.AfterFunc(h.every, h.tick)
	} else {
		h.timer.Reset(h.every)
	}
}

func (h *Heartbeat) tick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	s := int(time.Since(h.since).Seconds())
	h.report(fmt.Sprintf("still %s (%dm %02ds)", h.step, s/60, s%60))
	h.timer.Reset(h.every)
}

// Stop ends the still lines.
func (h *Heartbeat) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	if h.timer != nil {
		h.timer.Stop()
	}
}

// WhileRunning reports what, runs body, and reports a still line every
// interval until body returns.
func WhileRunning(what string, report func(string), every time.Duration, body func() error) error {
	h := Start(what, report, every)
	defer h.Stop()
	return body()
}

// Poll calls phase every interval and reports "<phase> (<elapsed>)" each
// time it changes, and "still <phase> (<elapsed>)" when it has not changed
// for every. elapsed counts from the call to Poll. report is never called
// after the returned stop returns.
func Poll(phase func() string, report func(string), interval, every time.Duration) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		start := time.Now()
		last, noted := "", start
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			now, p := time.Now(), phase()
			switch {
			case p != last:
				report(p + " (" + Duration(now.Sub(start)) + ")")
			case now.Sub(noted) >= every:
				report("still " + p + " (" + Duration(now.Sub(start)) + ")")
			default:
				continue
			}
			last, noted = p, now
		}
	})
	return func() {
		close(done)
		wg.Wait()
	}
}

// Duration is "45s", "6m" or "6m 05s".
func Duration(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s%60 == 0:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}
