// Package jobrun follows a guest job started through exec.start to its end
// over the exec.read cursor protocol, for glov's CLI and MCP tools and for
// glovd's own guest commands alike. It has no build tag, so its tests
// run on any host.
package jobrun

import (
	"context"
	"errors"
	"io"
	"time"
)

// Read is one exec.read answer, as the Job adapter got it.
type Read struct {
	Data []byte
	// Cursor is the offset in the job's whole output after Data.
	Cursor int
	Exited bool
	Code   *int
	// Lost: the guest rebooted, the VM stopped or the Guest Service was
	// replaced before the job exited.
	Lost bool
	// Dropped: output before the cursor fell out of glovd's window.
	Dropped bool
}

// Job is one started guest job.
type Job interface {
	// Read returns the job's output from cursor on.
	Read(ctx context.Context, cursor int) (Read, error)
	// Kill ends the job's process tree.
	Kill(ctx context.Context) error
}

// State is how a Run ended.
type State int

const (
	// Exited: the job exited and all of its output was written.
	Exited State = iota
	// Lost: the job's VM or Guest Service went away before it exited.
	Lost
	// Running: the wait ran out with the job still running; it was not
	// killed, and exec.read from Result.Cursor carries on.
	Running
)

// Result is how a Run ended.
type Result struct {
	State State
	// Code is the exit code when State is Exited.
	Code int
	// Cursor is the offset after the last output written.
	Cursor int
	// Dropped: some output fell out of glovd's window and was never
	// written.
	Dropped bool
}

// ErrNoCode is returned for a job that exited without an exit code, which
// glovd never reports.
var ErrNoCode = errors.New("the command exited without an exit code")

// Poll is how long Run waits before reading again when a read brought no
// new output.
const Poll = 100 * time.Millisecond

// killTimeout bounds the exec.kill sent when ctx ends.
const killTimeout = 5 * time.Second

// Run writes job's output to w until the job exits or is lost, or, when
// wait is positive, until wait has passed. When ctx ends first, Run kills
// the job and returns ctx's error. Errors from job are returned as they are.
func Run(ctx context.Context, job Job, w io.Writer, wait time.Duration) (Result, error) {
	var deadline time.Time
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	var res Result
	for {
		r, err := job.Read(ctx, res.Cursor)
		if ctx.Err() != nil {
			return res, kill(ctx, job)
		}
		if err != nil {
			return res, err
		}
		res.Dropped = res.Dropped || r.Dropped
		w.Write(r.Data)
		progressed := r.Cursor != res.Cursor
		res.Cursor = r.Cursor
		switch {
		case r.Lost:
			res.State = Lost
			return res, nil
		case r.Exited && !progressed:
			// glovd sets Exited as soon as the job ends, while output
			// may still wait to be read: only an empty read is the end.
			if r.Code == nil {
				return res, ErrNoCode
			}
			res.State, res.Code = Exited, *r.Code
			return res, nil
		case !deadline.IsZero() && time.Now().After(deadline):
			res.State = Running
			return res, nil
		case progressed:
			continue
		}
		select {
		case <-ctx.Done():
			return res, kill(ctx, job)
		case <-time.After(Poll):
		}
	}
}

// kill ends job after ctx ended, and returns ctx's error.
func kill(ctx context.Context, job Job) error {
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killTimeout)
	defer cancel()
	job.Kill(kctx)
	return ctx.Err()
}
