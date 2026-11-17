package mirror_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/internal/mirror"
)

type watchFixture struct {
	*fixture
	changes chan mirror.Changed
}

func newWatch(t *testing.T, exclude ...mirror.Rule) *watchFixture {
	return newWatchDebounced(t, 20*time.Millisecond, 200*time.Millisecond, exclude...)
}

func newWatchDebounced(t *testing.T, settle, maxDelay time.Duration, exclude ...mirror.Rule) *watchFixture {
	t.Cleanup(mirror.SetDebounce(settle, maxDelay))
	f := &watchFixture{newFixture(t), make(chan mirror.Changed, 16)}
	f.m.SetNotify(func(method string, params any) {
		if method == "sync.changed" {
			f.changes <- params.(mirror.Changed)
		}
	})
	if _, err := f.m.Watch(mirror.WatchParams{ID: "proj", Root: f.root, Exclude: exclude}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.m.Unwatch(mirror.StatusParams{ID: "proj"}) })
	return f
}

// next collects what is reported until nothing more comes for a while.
func (f *watchFixture) next() (paths []string, overflow bool) {
	f.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case c := <-f.changes:
			paths = append(paths, c.Paths...)
			overflow = overflow || c.Overflow
			timeout = time.After(400 * time.Millisecond)
		case <-timeout:
			slices.Sort(paths)
			return slices.Compact(paths), overflow
		}
	}
}

func TestAWatchReportsGuestChanges(t *testing.T) {
	f := newWatch(t, mirror.Rule{Pattern: `(^|/)node_modules$`, DirOnly: true})
	f.put("a.txt", "a")
	f.put("d/e/b.txt", "b") // made before its directories' watches exist
	f.put("node_modules/x/y.js", "y")
	if paths, _ := f.next(); !slices.Equal(paths, []string{"a.txt", "d", "d/e", "d/e/b.txt"}) {
		t.Errorf("reported %v", paths)
	}
	if err := os.Rename(filepath.Join(f.root, "d"), filepath.Join(f.root, "moved")); err != nil {
		t.Fatal(err)
	}
	f.put("moved/e/c.txt", "c") // watched again, at its new path
	if paths, _ := f.next(); !slices.Equal(paths, []string{"d", "moved", "moved/e", "moved/e/b.txt", "moved/e/c.txt"}) {
		t.Errorf("after the move reported %v", paths)
	}
	if err := os.Remove(filepath.Join(f.root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if paths, _ := f.next(); !slices.Equal(paths, []string{"a.txt"}) {
		t.Errorf("after the delete reported %v", paths)
	}
}

func TestAWatchLeavesOutTheGuestServicesOwnWrites(t *testing.T) {
	f := newWatch(t)
	f.apply(true, write("host.txt", "from the host"), mirror.Op{Op: "mkdir", Path: "dir"})
	paths, _ := f.next()
	// The directory is reported (directories have no records), the file not.
	if !slices.Equal(paths, []string{"dir"}) {
		t.Errorf("reported %v, want just dir", paths)
	}
	f.apply(true, mirror.Op{Op: "delete", Path: "host.txt"})
	if paths, _ := f.next(); len(paths) != 0 {
		t.Errorf("after the host's delete reported %v", paths)
	}
	f.put("host.txt", "a guest edit")
	if paths, _ := f.next(); !slices.Equal(paths, []string{"host.txt"}) {
		t.Errorf("after a guest edit reported %v", paths)
	}
}

func TestAWatchNeverFollowsALink(t *testing.T) {
	outside := t.TempDir()
	f := newWatch(t)
	plantLink(t, outside, filepath.Join(f.root, "link"))
	f.next()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if paths, _ := f.next(); len(paths) != 0 {
		t.Errorf("a change behind a link was reported: %v", paths)
	}
}

func TestManyChangesAreAnOverflow(t *testing.T) {
	f := newWatchDebounced(t, time.Second, 10*time.Second) // one report for them all
	for i := range 1200 {
		f.put(fmt.Sprintf("many/f%04d", i), "x")
	}
	if _, overflow := f.next(); !overflow {
		t.Error("no overflow for 1200 changes")
	}
}

func TestADeletedRootEndsTheWatch(t *testing.T) {
	f := newWatch(t)
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-f.changes:
		if !c.Overflow || c.Error == "" {
			t.Errorf("got %+v, want an overflow with an error", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification for the deleted root")
	}
	// Watching it again recreates it.
	if _, err := f.m.Watch(mirror.WatchParams{ID: "proj", Root: f.root}); err != nil {
		t.Fatal(err)
	}
}

func TestReadReturnsAFileInChunks(t *testing.T) {
	f := newFixture(t)
	big := strings.Repeat("0123456789abcdef", 40<<10) // 640 KiB: two chunks
	f.put("big", big)
	var got []byte
	var key string
	for off := int64(0); ; {
		r, err := f.m.Read(mirror.ReadParams{ID: "proj", Root: f.root, Path: "big", Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		if key != "" && r.Key != key || r.Size != int64(len(big)) {
			t.Fatalf("chunk at %d: key %q size %d", off, r.Key, r.Size)
		}
		key = r.Key
		got = append(got, r.Data...)
		off += int64(len(r.Data))
		if r.EOF {
			break
		}
	}
	if string(got) != big {
		t.Error("the chunks do not make up the file")
	}
}

func TestReadRefusesLinksAndSpecialFiles(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.put("d/ok", "ok")
	plantLink(t, filepath.Join(outside, "secret"), filepath.Join(f.root, "file-link"))
	plantLink(t, outside, filepath.Join(f.root, "dir-link"))
	if err := syscall.Mkfifo(filepath.Join(f.root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range []string{"file-link", "dir-link/secret", "fifo", "d", "gone"} {
			if _, err := f.m.Read(mirror.ReadParams{ID: "proj", Root: f.root, Path: p}); err == nil {
				t.Errorf("read %s: no error", p)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read blocked")
	}
}
