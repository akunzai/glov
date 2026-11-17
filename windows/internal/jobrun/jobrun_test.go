package jobrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeJob answers reads from a script; past its end it repeats the last
// read with no data at the same cursor.
type fakeJob struct {
	mu      sync.Mutex
	reads   []Read
	cursors []int
	killed  bool
	killCtx error
	err     error
}

func (f *fakeJob) Read(ctx context.Context, cursor int) (Read, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors = append(f.cursors, cursor)
	if f.err != nil {
		return Read{}, f.err
	}
	if len(f.reads) == 0 {
		return Read{Cursor: cursor}, nil
	}
	r := f.reads[0]
	if len(f.reads) > 1 {
		f.reads = f.reads[1:]
	} else {
		f.reads[0].Data = nil
	}
	return r, nil
}

func (f *fakeJob) Kill(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed, f.killCtx = true, ctx.Err()
	return nil
}

func code(n int) *int { return &n }

func TestRunReadsEveryPageLeftAfterTheJobExited(t *testing.T) {
	job := &fakeJob{reads: []Read{
		{Data: []byte("a\n"), Cursor: 2, Exited: true, Code: code(3)},
		{Data: []byte("b\n"), Cursor: 4, Exited: true, Code: code(3)},
		{Data: []byte("c\n"), Cursor: 6, Exited: true, Code: code(3)},
		{Cursor: 6, Exited: true, Code: code(3)},
	}}
	var out strings.Builder
	res, err := Run(context.Background(), job, &out, 0)
	if err != nil || res.State != Exited || res.Code != 3 || res.Cursor != 6 || out.String() != "a\nb\nc\n" {
		t.Errorf("Run = %+v, %v, wrote %q", res, err, out.String())
	}
	if len(job.cursors) != 4 || job.killed {
		t.Errorf("read at %v, killed %v", job.cursors, job.killed)
	}
}

func TestRunReportsALostJobAfterItsLastOutput(t *testing.T) {
	job := &fakeJob{reads: []Read{
		{Data: []byte("partial"), Cursor: 7, Lost: true},
	}}
	var out strings.Builder
	res, err := Run(context.Background(), job, &out, 0)
	if err != nil || res.State != Lost || out.String() != "partial" {
		t.Errorf("Run = %+v, %v, wrote %q", res, err, out.String())
	}
}

func TestRunReportsDroppedOutput(t *testing.T) {
	job := &fakeJob{reads: []Read{
		{Data: []byte("late"), Cursor: 100, Dropped: true},
		{Cursor: 100, Exited: true, Code: code(0)},
	}}
	res, err := Run(context.Background(), job, &strings.Builder{}, 0)
	if err != nil || !res.Dropped || res.State != Exited {
		t.Errorf("Run = %+v, %v", res, err)
	}
}

func TestRunReturnsARunningJobAfterTheWaitWithoutKillingIt(t *testing.T) {
	job := &fakeJob{reads: []Read{{Data: []byte("tick\n"), Cursor: 5}}}
	var out strings.Builder
	res, err := Run(context.Background(), job, &out, 3*Poll)
	if err != nil || res.State != Running || res.Cursor != 5 || out.String() != "tick\n" || job.killed {
		t.Errorf("Run = %+v, %v, wrote %q, killed %v", res, err, out.String(), job.killed)
	}
}

func TestRunKillsTheJobWithAFreshContextWhenCtxEnds(t *testing.T) {
	job := &fakeJob{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(2*Poll, cancel)
	_, err := Run(ctx, job, &strings.Builder{}, 0)
	if !errors.Is(err, context.Canceled) || !job.killed || job.killCtx != nil {
		t.Errorf("Run = %v, killed %v with ctx error %v", err, job.killed, job.killCtx)
	}
}

func TestRunKillsTheJobWhenAReadFailsBecauseCtxEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := &fakeJob{err: context.Canceled}
	if _, err := Run(ctx, job, &strings.Builder{}, 0); !errors.Is(err, context.Canceled) || !job.killed {
		t.Errorf("Run = %v, killed %v", err, job.killed)
	}
}

func TestRunReturnsTheAdaptersErrorAsItIs(t *testing.T) {
	want := errors.New("VM dev is not running")
	job := &fakeJob{err: want}
	if _, err := Run(context.Background(), job, &strings.Builder{}, 0); err != want || job.killed {
		t.Errorf("Run = %v, killed %v", err, job.killed)
	}
}

func TestRunRefusesAnExitWithoutACode(t *testing.T) {
	job := &fakeJob{reads: []Read{{Cursor: 0, Exited: true}}}
	if _, err := Run(context.Background(), job, &strings.Builder{}, 0); !errors.Is(err, ErrNoCode) {
		t.Errorf("Run = %v", err)
	}
}
