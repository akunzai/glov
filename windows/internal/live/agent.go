package live

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

// Agent runs an Engine for as long as a live share is kept in step: a full
// round first, then a round for the paths either side reports, a full one
// after an overflow and every FullEvery as a safety net.
type Agent struct {
	Engine *Engine
	// Save persists the engine's base (and the host's hash cache).
	Save func() error
	// Conflict is told each path both sides changed.
	Conflict func(Conflict)
	// Stale, if set, is told the git lock files (Result.Locks) held longer
	// than StaleLock whenever that list changes: git that crashed leaves
	// one behind, which holds its directory until someone deletes it.
	Stale     func(locks []string)
	StaleLock time.Duration
	// InStep, if set, is called once the first full round is done.
	InStep func()
	Log    *slog.Logger
	// Settle is how long changes are gathered before a round; FullEvery
	// how often a full round runs regardless; Retry how long a failed path
	// waits, at most Tries times in a row.
	Settle, FullEvery, Retry time.Duration
	Tries                    int
	// Recover, if set, is called when a round fails, before it is tried
	// again: it may wait (for a Guest Service that reconnects) and says
	// whether the cause is gone, or returns an error to stop Run with.
	Recover func(err error) (recovered bool, stop error)

	mu    sync.Mutex
	paths map[string]bool
	full  bool
	kick  chan struct{}
	tries map[string]int
	// locks are when each lock file of the last rounds was first seen;
	// stale what Stale was last told.
	locks map[string]time.Time
	stale []string
}

func (a *Agent) init() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kick == nil {
		a.kick = make(chan struct{}, 1)
		a.paths, a.tries = map[string]bool{}, map[string]int{}
	}
}

func (a *Agent) wake() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Changed queues paths for the next round.
func (a *Agent) Changed(paths []string) {
	a.init()
	a.mu.Lock()
	for _, p := range paths {
		a.paths[p] = true
	}
	a.mu.Unlock()
	a.wake()
}

// Full queues a full round.
func (a *Agent) Full() {
	a.init()
	a.mu.Lock()
	a.full = true
	a.mu.Unlock()
	a.wake()
}

// maxErrors is how many rounds in a row may fail before Run gives up.
const maxErrors = 5

// Run runs rounds until ctx ends, or returns the error of the last of
// maxErrors rounds that failed in a row (glovd or the VM went away).
func (a *Agent) Run(ctx context.Context) error {
	a.init()
	a.Full()
	full := time.NewTicker(a.FullEvery)
	defer full.Stop()
	failed := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-full.C:
			a.Full()
			continue
		case <-a.kick:
		}
		select { // gather what comes with it
		case <-ctx.Done():
			return nil
		case <-time.After(a.Settle):
		}
		a.mu.Lock()
		isFull, paths := a.full, slices.Sorted(maps.Keys(a.paths))
		a.full, a.paths = false, map[string]bool{}
		a.mu.Unlock()
		started := time.Now()
		res, err := a.Engine.Round(isFull, paths)
		if err != nil {
			failed++
			a.Log.Warn("live share round failed", "full", isFull, "err", err)
			if a.Recover != nil {
				recovered, stop := a.Recover(err)
				if stop != nil {
					return stop
				}
				if recovered {
					failed, isFull = 0, true // the guest may have changed meanwhile
				}
			}
			if failed >= maxErrors {
				return err
			}
			a.mu.Lock()
			a.full = a.full || isFull
			for _, p := range paths {
				a.paths[p] = true
			}
			a.mu.Unlock()
			time.AfterFunc(a.Retry, a.wake)
			continue
		}
		failed = 0
		if err := a.Save(); err != nil {
			a.Log.Warn("live share state not saved", "err", err)
		}
		for _, c := range res.Conflicts {
			a.Conflict(c)
		}
		a.watchLocks(res.Locks, started)
		if isFull && a.InStep != nil {
			a.InStep()
			a.InStep = nil
		}
		a.retry(paths, res.Retry)
		if res.ToGuest+res.ToHost+len(res.Retry) > 0 || isFull {
			a.Log.Info("live share round", "full", isFull, "toGuest", res.ToGuest, "toHost", res.ToHost,
				"retry", len(res.Retry), "seconds", time.Since(started).Seconds())
		}
	}
}

// watchLocks tells Stale of the lock files seen since StaleLock ago.
func (a *Agent) watchLocks(locks []string, now time.Time) {
	seen := map[string]time.Time{}
	var stale []string
	for _, l := range locks {
		t, ok := a.locks[l]
		if !ok {
			t = now
		}
		seen[l] = t
		if now.Sub(t) >= a.StaleLock {
			stale = append(stale, l)
		}
	}
	a.locks = seen
	if slices.Equal(stale, a.stale) {
		return
	}
	a.stale = stale
	if len(stale) > 0 {
		a.Log.Warn("git lock files hold the share's git data", "locks", stale)
	}
	if a.Stale != nil {
		a.Stale(stale)
	}
}

// retry queues the paths that failed again after a.Retry, each at most
// a.Tries times in a row; the paths of the round that went through start
// counting afresh. A path the host refused (ErrName, ErrExcluded) is
// logged, not queued: it would be refused again, until the guest changes it.
func (a *Agent) retry(paths []string, failed map[string]error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range paths {
		if failed[p] == nil {
			delete(a.tries, p)
		}
	}
	var again, refused, excluded []string
	for p, err := range failed {
		if errors.Is(err, ErrName) {
			refused = append(refused, p)
			delete(a.tries, p)
			continue
		}
		if errors.Is(err, ErrExcluded) {
			excluded = append(excluded, p)
			delete(a.tries, p)
			continue
		}
		if errors.Is(err, ErrBusy) {
			// Git is at work: wait for it, however long.
			again = append(again, p)
			continue
		}
		a.tries[p]++
		if a.tries[p] > a.Tries {
			a.Log.Warn("live share path left until it changes again", "path", p, "err", err)
			delete(a.tries, p)
			continue
		}
		again = append(again, p)
	}
	if len(refused) > 0 {
		// The guest named them; it may name many, so the log shows a few.
		slices.Sort(refused)
		a.Log.Warn("live share paths refused: not names a Windows folder can hold as themselves",
			"count", len(refused), "paths", refused[:min(len(refused), 10)])
	}
	if len(excluded) > 0 {
		slices.Sort(excluded)
		a.Log.Warn("live share paths refused: excluded from the share",
			"count", len(excluded), "paths", excluded[:min(len(excluded), 10)])
	}
	if len(again) > 0 {
		time.AfterFunc(a.Retry, func() { a.Changed(again) })
	}
}
