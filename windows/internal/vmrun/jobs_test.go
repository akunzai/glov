package vmrun

import (
	"bytes"
	"errors"
	"testing"
)

func TestJobOutputBeforeStartAndCollection(t *testing.T) {
	j := newJobs()
	j.output(7, []byte("early "))
	id := j.started(7, "b1")
	j.output(7, []byte("late"))
	r, err := j.read(id, 0, 1<<20)
	if err != nil || string(r.Data) != "early late" || r.Cursor != 10 || r.Exited {
		t.Fatalf("read = %+v, %v", r, err)
	}
	if r, _ := j.read(id, 3, 4); string(r.Data) != "ly l" || r.Cursor != 7 {
		t.Fatalf("partial read = %+v", r)
	}
	j.exit(7, 3)
	// PID 7 may be reused by a new job now.
	j.output(7, []byte("other"))
	r, _ = j.read(id, 10, 100)
	if !r.Exited || *r.Code != 3 || len(r.Data) != 0 {
		t.Fatalf("final read = %+v", r)
	}
	if _, err := j.read(id, 10, 100); !errors.Is(err, ErrNoJob) {
		t.Fatalf("read after collection err = %v", err)
	}
}

func TestJobExitBeforeStartAnswer(t *testing.T) {
	j := newJobs()
	j.output(9, []byte("x"))
	j.exit(9, 0)
	id := j.started(9, "b1")
	if r, _ := j.read(id, 0, 10); !r.Exited || string(r.Data) != "x" {
		t.Fatalf("read = %+v", r)
	}
	if _, ok := j.byPID[9]; ok {
		t.Fatal("an exited job stays in byPID")
	}
}

func TestJobWindowDropsOldOutput(t *testing.T) {
	j := newJobs()
	id := j.started(1, "b1")
	j.output(1, bytes.Repeat([]byte("a"), OutputWindow))
	j.output(1, []byte("tail"))
	r, _ := j.read(id, 0, 8)
	// The first 4 bytes fell out of the window.
	if !r.Dropped || string(r.Data) != "aaaaaaaa" || r.Cursor != 4+8 {
		t.Fatalf("read = dropped %v data %q cursor %d", r.Dropped, r.Data, r.Cursor)
	}
	if r, _ := j.read(id, OutputWindow, 100); r.Dropped || string(r.Data) != "tail" {
		t.Fatalf("read at the end = %+v", r)
	}
}

func TestJobsOfAnotherBootAreLost(t *testing.T) {
	j := newJobs()
	old := j.started(1, "b1")
	cur := j.started(2, "b2")
	j.rebooted("b2")
	if r, _ := j.read(old, 0, 10); !r.Lost {
		t.Error("old boot's job not lost")
	}
	if r, _ := j.read(cur, 0, 10); r.Lost {
		t.Error("current boot's job lost")
	}
	if _, err := j.pid(old); err == nil {
		t.Error("pid of a lost job")
	}
	j.stopped()
	if r, _ := j.read(cur, 0, 10); !r.Lost {
		t.Error("job not lost when the VM stopped")
	}
}

// A guest naming PIDs no exec.start answered with must not make glovd
// hold much for them.
func TestNotificationsForUnknownPIDsAreBounded(t *testing.T) {
	j := newJobs()
	big := bytes.Repeat([]byte("x"), 1<<20)
	for pid := 1000; pid < 1100; pid++ {
		j.output(pid, big)
		j.exit(pid, 0)
	}
	if len(j.pending) != pendingPIDs || len(j.byPID) != 0 || len(j.byID) != 0 {
		t.Fatalf("pending %d, byPID %d, byID %d", len(j.pending), len(j.byPID), len(j.byID))
	}
	for _, jb := range j.pending {
		if len(jb.buf) > pendingWindow {
			t.Fatalf("pending job holds %d bytes", len(jb.buf))
		}
	}
	// An expired pending PID makes room again.
	for _, jb := range j.pending {
		jb.changed = jb.changed.Add(-2 * pendingTTL)
	}
	j.output(5, []byte("hi"))
	id := j.started(5, "b1")
	if r, _ := j.read(id, 0, 10); string(r.Data) != "hi" {
		t.Fatalf("read = %+v", r)
	}
}
