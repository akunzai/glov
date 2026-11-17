package vmrun

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// OutputWindow is how much of a job's output glovd keeps; a reader
// that falls further behind skips ahead and is told so (dropped). The
// Guest Service streams output without flow control and glovd keeps
// no log users could read (its data directory is SYSTEM's), so the window
// is large: seq 1 300000 (2 MB at once) lost half its lines to 1 MiB.
const OutputWindow = 16 << 20

// jobRetention is how long a finished job nobody reads to the end is kept.
const jobRetention = time.Hour

// Output for a PID no exec.start has answered with yet is held, briefly
// and in small amounts: the guest is not trusted to name only real jobs,
// and each name must not cost glovd a 16 MiB window.
const (
	pendingPIDs   = 16
	pendingWindow = 64 << 10
	pendingTTL    = 10 * time.Second
)

// ErrNoJob is returned for an unknown or already collected job.
var ErrNoJob = errors.New("no such job (it finished and its output was read, or glovd restarted)")

// Read is one exec.read answer.
type Read struct {
	Data []byte
	// Cursor is the offset in the job's whole output after Data.
	Cursor  int
	Exited  bool
	Code    *int
	Lost    bool
	Dropped bool
}

// job is a guest process started through exec.start, with its output.
type job struct {
	id      string
	pid     int
	bootID  string
	buf     []byte // the last OutputWindow bytes of output
	base    int    // offset of buf[0] in the whole output
	exited  bool
	code    *int
	lost    bool
	changed time.Time
}

// jobs tracks one VM's jobs: by ID for glov, and by PID for the Guest
// Service's notifications while they run.
type jobs struct {
	mu      sync.Mutex
	byID    map[string]*job
	byPID   map[int]*job // registered jobs still running
	pending map[int]*job // notifications ahead of exec.start's answer
}

func newJobs() *jobs {
	return &jobs{byID: map[string]*job{}, byPID: map[int]*job{}, pending: map[int]*job{}}
}

// running counts the jobs that have not exited.
func (j *jobs) running() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.byPID)
}

// forPID returns the job a notification for pid belongs to: a registered
// one, or a pending one that exec.start's answer will claim. It returns
// nil, dropping the notification, when too many PIDs are pending.
func (j *jobs) forPID(pid int) *job {
	if jb, ok := j.byPID[pid]; ok {
		jb.changed = time.Now()
		return jb
	}
	jb, ok := j.pending[pid]
	if !ok {
		for p, old := range j.pending {
			if time.Since(old.changed) > pendingTTL {
				delete(j.pending, p)
			}
		}
		if len(j.pending) >= pendingPIDs {
			return nil
		}
		jb = &job{pid: pid}
		j.pending[pid] = jb
	}
	jb.changed = time.Now()
	return jb
}

// started registers the job exec.start answered with pid, on bootID.
func (j *jobs) started(pid int, bootID string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.prune()
	jb, ok := j.pending[pid]
	if ok {
		delete(j.pending, pid)
	} else {
		jb = &job{pid: pid}
	}
	b := make([]byte, 8)
	rand.Read(b)
	jb.id, jb.bootID, jb.changed = hex.EncodeToString(b), bootID, time.Now()
	j.byID[jb.id] = jb
	if !jb.exited {
		j.byPID[pid] = jb
	}
	return jb.id
}

func (j *jobs) output(pid int, data []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb := j.forPID(pid)
	if jb == nil {
		return
	}
	window := OutputWindow
	if jb.id == "" {
		window = pendingWindow
	}
	jb.buf = append(jb.buf, data...)
	if over := len(jb.buf) - window; over > 0 {
		jb.buf = append([]byte(nil), jb.buf[over:]...)
		jb.base += over
	}
}

func (j *jobs) exit(pid, code int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb := j.forPID(pid)
	if jb == nil {
		return
	}
	jb.exited, jb.code = true, &code
	if jb.id != "" {
		delete(j.byPID, pid)
	}
}

// rebooted marks the jobs of other boots that never exited as lost: the
// guest that ran them is gone.
func (j *jobs) rebooted(bootID string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for pid, jb := range j.byPID {
		if jb.bootID != bootID {
			jb.lost, jb.changed = true, time.Now()
			delete(j.byPID, pid)
		}
	}
	clear(j.pending)
}

// stopped marks every running job lost, when the VM stops.
func (j *jobs) stopped() { j.rebooted("\x00") }

// read returns output from cursor on, at most max bytes. A read that finds
// the job ended and nothing more to say collects it.
func (j *jobs) read(id string, cursor, max int) (Read, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb, ok := j.byID[id]
	if !ok {
		return Read{}, ErrNoJob
	}
	r := Read{Exited: jb.exited, Code: jb.code, Lost: jb.lost}
	if cursor < jb.base {
		cursor, r.Dropped = jb.base, true
	}
	end := jb.base + len(jb.buf)
	cursor = min(cursor, end)
	r.Data = append([]byte(nil), jb.buf[cursor-jb.base:min(end, cursor+max)-jb.base]...)
	r.Cursor = cursor + len(r.Data)
	if (jb.exited || jb.lost) && len(r.Data) == 0 {
		delete(j.byID, id)
	}
	return r, nil
}

// pid is the running job id's guest PID.
func (j *jobs) pid(id string) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb, ok := j.byID[id]
	if !ok {
		return 0, ErrNoJob
	}
	if jb.exited || jb.lost {
		return 0, errors.New("the job has ended")
	}
	return jb.pid, nil
}

// prune drops finished jobs nobody collected; j.mu is held.
func (j *jobs) prune() {
	for id, jb := range j.byID {
		if (jb.exited || jb.lost) && time.Since(jb.changed) > jobRetention {
			delete(j.byID, id)
		}
	}
}
