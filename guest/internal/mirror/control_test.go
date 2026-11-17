package mirror_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/internal/mirror"
)

// blockingReader stands for a read that does not return until released, as a
// guest file can hold one up; started is closed once the read begins.
type blockingReader struct {
	io.ReadCloser
	started, release chan struct{}
}

func (r blockingReader) Read(b []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	<-r.release
	return r.ReadCloser.Read(b)
}

// blockOn makes hashing rel block until release is closed.
func blockOn(t *testing.T, rel string) (started, release chan struct{}) {
	started, release = make(chan struct{}), make(chan struct{})
	t.Cleanup(mirror.SetOpenForHash(func(r *os.Root, p string) (io.ReadCloser, error) {
		f, err := r.Open(p)
		if err != nil || filepath.ToSlash(p) != rel {
			return f, err
		}
		return blockingReader{f, started, release}, nil
	}))
	return started, release
}

func TestStatusNamesTheFileAListingIsStuckOnAndCancelStopsIt(t *testing.T) {
	f := newFixture(t)
	f.put("a.txt", "a")
	f.put("logs/held.log", "held")
	f.manifest(mirror.ManifestParams{})
	f.apply(true)
	f.put("logs/held.log", "changed") // rehashed by the next listing
	started, release := blockOn(t, "logs/held.log")

	done := make(chan error, 1)
	go func() {
		_, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root})
		done <- err
	}()
	<-started
	st, err := f.m.Status(mirror.StatusParams{ID: "proj"})
	if err != nil || !st.Busy || st.Phase != "manifest" || st.Path != "logs/held.log" {
		t.Fatalf("status = %+v, %v; want busy in manifest on logs/held.log", st, err)
	}

	if _, err := f.m.Cancel(mirror.StatusParams{ID: "proj"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled listing = %v, want a cancelled error", err)
	}
	if st, _ := f.m.Status(mirror.StatusParams{ID: "proj"}); st.Busy {
		t.Fatalf("status after cancel = %+v, want idle", st)
	}

	// The next listing starts afresh and still knows the unchanged file.
	entries, r := f.manifest(mirror.ManifestParams{})
	if r.Hashed != 1 || entries["logs/held.log"].Hash != sum("changed") || entries["a.txt"].Hash != sum("a") {
		t.Fatalf("listing after cancel: hashed %d, entries %+v", r.Hashed, entries)
	}
}

func TestACallStuckInTheGuestIsReportedInsteadOfWaitedFor(t *testing.T) {
	t.Cleanup(mirror.SetLockWait(50 * time.Millisecond))
	f := newFixture(t)
	f.put("held.log", "held")
	started, release := blockOn(t, "held.log")

	done := make(chan error, 1)
	go func() {
		_, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root})
		done <- err
	}()
	<-started
	f.m.Cancel(mirror.StatusParams{ID: "proj"}) // the read is blocked in the OS: cancel cannot end it

	for _, call := range []func() error{
		func() error { _, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root}); return err },
		func() error { _, err := f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root}); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "held.log") {
			t.Fatalf("call while stuck = %v, want an error naming held.log", err)
		}
	}

	close(release)
	<-done
	if _, r := f.manifest(mirror.ManifestParams{}); r.Hashed != 1 {
		t.Fatalf("hashed %d after the stuck read returned, want 1", r.Hashed)
	}
}

func TestCancelStopsABatchBeforeItsNextOperation(t *testing.T) {
	f := newFixture(t)
	f.manifest(mirror.ManifestParams{})
	f.m.Cancel(mirror.StatusParams{ID: "proj"})
	if _, err := f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Ops: []mirror.Op{write("a.txt", "a")}}); err == nil {
		t.Fatal("apply after cancel succeeded")
	}
	if _, ok := f.read("a.txt"); ok {
		t.Fatal("a cancelled batch wrote a.txt")
	}
	// A new sync starts with a listing, which clears the cancel.
	f.manifest(mirror.ManifestParams{})
	if failures := f.apply(true, write("a.txt", "a")); len(failures) != 0 {
		t.Fatalf("failures %+v", failures)
	}
}

// A cancel ends the writes in progress too: no temporary file is left, and
// the root is closed, which on Windows lets the share folder be renamed.
func TestCancelDropsWritesInProgressAndReleasesTheRoot(t *testing.T) {
	f := newFixture(t)
	f.manifest(mirror.ManifestParams{})
	f.apply(false, mirror.Op{Op: "write", Path: "big.bin", Data: []byte("first chunk")})
	if len(tempFiles(t, f.root)) != 1 {
		t.Fatal("the first chunk left no temporary file")
	}
	f.m.Cancel(mirror.StatusParams{ID: "proj"})
	if _, err := f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Ops: []mirror.Op{write("a.txt", "a")}}); err == nil {
		t.Fatal("apply after cancel succeeded")
	}
	if left := tempFiles(t, f.root); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
	if err := os.Rename(f.root, f.root+".moved"); err != nil {
		t.Fatalf("the share folder is still held after the cancel: %v", err)
	}
}
