package mirror

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akunzai/glov/guest/proto"
)

// lockWait is how long a call waits for the share's previous call. The host
// sends one call at a time per share, so a call still running after that is
// one the host gave up on, stuck in the guest (a read that never returns).
var lockWait = 10 * time.Second

// slowHash is how long hashing one file may take before it is logged.
const slowHash = 30 * time.Second

// errCancelled ends a walk or a batch after sync.cancel.
var errCancelled = errors.New("cancelled by the host")

// progress is what sync.status reports about the call running for a share.
// It is guarded by its own lock, not the session's, so that it can be read
// while that call runs.
type progress struct {
	mu      sync.Mutex
	phase   string // "manifest" or "apply"; empty when idle
	path    string // the file being hashed or written
	started time.Time
	files   atomic.Int64 // hashed or written in this call
	bytes   atomic.Int64 // read for hashing or written in this call
}

func (p *progress) begin(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase, p.path, p.started = phase, "", time.Now()
	p.files.Store(0)
	p.bytes.Store(0)
}

func (p *progress) end() { p.begin("") }

func (p *progress) at(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = path
}

// lock takes the session for one call, or fails naming what holds it.
func (s *session) lock(id string) error {
	select {
	case s.busy <- struct{}{}:
		return nil
	case <-time.After(lockWait):
		st := s.status()
		slog.Warn("sync still running", "id", id, "phase", st.Phase, "path", st.Path, "seconds", st.Seconds)
		return proto.Errorf(proto.CodeOS, "an earlier sync of share %s is still running in the guest (%s of %q for %ds); "+
			"wait for it, or restart the Guest Service", id, st.Phase, st.Path, int(st.Seconds))
	}
}

func (s *session) unlock() { <-s.busy }

// cancelled reports whether sync.cancel arrived since the walk began.
func (s *session) cancelled() bool { return s.cancel.Load() }

// StatusParams are sync.status's and sync.cancel's params.
type StatusParams struct {
	ID string `json:"id"`
}

// Status is sync.status's result.
type Status struct {
	Busy    bool    `json:"busy"`
	Phase   string  `json:"phase,omitempty"`
	Path    string  `json:"path,omitempty"`
	Files   int64   `json:"files"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
}

func (s *session) status() Status {
	p := &s.progress
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.phase == "" {
		return Status{}
	}
	return Status{Busy: true, Phase: p.phase, Path: p.path, Files: p.files.Load(), Bytes: p.bytes.Load(), Seconds: time.Since(p.started).Seconds()}
}

// Status reports the call running for a share, so the host can tell a long
// listing from a stuck one.
func (m *Manager) Status(p StatusParams) (Status, error) {
	if err := checkID(p.ID); err != nil {
		return Status{}, err
	}
	return m.session(p.ID).status(), nil
}

// Cancel asks the share's running call to stop at the next file or chunk. The
// next listing clears it. A read blocked in the OS is not interrupted: the
// call then keeps the share until it returns, and later calls say so (lock).
func (m *Manager) Cancel(p StatusParams) (struct{}, error) {
	if err := checkID(p.ID); err != nil {
		return struct{}{}, err
	}
	s := m.session(p.ID)
	s.cancel.Store(true)
	if st := s.status(); st.Busy {
		slog.Info("sync cancelled", "id", p.ID, "phase", st.Phase, "path", st.Path, "seconds", st.Seconds)
	}
	return struct{}{}, nil
}

// progressReader counts what is read for hashing and stops once cancelled.
type progressReader struct {
	r io.Reader
	s *session
}

func (r progressReader) Read(b []byte) (int, error) {
	if r.s.cancelled() {
		return 0, errCancelled
	}
	n, err := r.r.Read(b)
	r.s.progress.bytes.Add(int64(n))
	return n, err
}
