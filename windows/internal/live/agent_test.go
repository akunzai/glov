package live

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestALockHeldLongIsReportedUntilItGoes(t *testing.T) {
	var told [][]string
	a := &Agent{Log: slog.New(slog.DiscardHandler), StaleLock: time.Minute, Stale: func(l []string) { told = append(told, l) }}
	t0 := time.Unix(0, 0)
	a.watchLocks([]string{"host .git/index.lock"}, t0)
	a.watchLocks([]string{"host .git/index.lock"}, t0.Add(30*time.Second))
	a.watchLocks([]string{"host .git/index.lock"}, t0.Add(time.Minute))
	a.watchLocks([]string{"host .git/index.lock"}, t0.Add(2*time.Minute))
	a.watchLocks(nil, t0.Add(3*time.Minute))
	if len(told) != 2 || len(told[0]) != 1 || told[1] != nil {
		t.Errorf("told %q", told)
	}
}

func TestARefusedNameIsLoggedAndNotTriedAgain(t *testing.T) {
	var log bytes.Buffer
	a := &Agent{Log: slog.New(slog.NewTextHandler(&log, nil)), Tries: 3}
	a.init()
	a.retry([]string{"x.", "secret/k", "a/b"}, map[string]error{
		"x.":       fmt.Errorf("%q: %w", "x.", ErrName),
		"secret/k": fmt.Errorf("%q: %w", "secret/k", ErrExcluded),
		"a/b":      ErrConflict,
	})
	for deadline := time.Now().Add(5 * time.Second); ; {
		a.mu.Lock()
		queued, refused := a.paths["a/b"], a.paths["x."] || a.paths["secret/k"]
		a.mu.Unlock()
		if refused {
			t.Fatal("a refused path was queued again")
		}
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the conflict was not queued again")
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(log.String(), "level=WARN") || !strings.Contains(log.String(), `x.`) ||
		!strings.Contains(log.String(), "excluded from the share") || !strings.Contains(log.String(), "secret/k") {
		t.Errorf("log %q", log.String())
	}
}
