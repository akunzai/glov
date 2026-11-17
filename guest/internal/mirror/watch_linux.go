package mirror

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/akunzai/glov/guest/proto"
	"golang.org/x/sys/unix"
)

// Debouncing: changes are reported once none has come for settle, and at
// least every maxDelay while they keep coming.
var (
	settle   = 150 * time.Millisecond
	maxDelay = time.Second
)

// maxChanged bounds the paths of one sync.changed; more make it an overflow.
const maxChanged = 1000

const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MODIFY | unix.IN_CLOSE_WRITE | unix.IN_ATTRIB |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF |
	unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW | unix.IN_EXCL_UNLINK

// WatchParams are sync.watch's params.
type WatchParams struct {
	ID      string `json:"id"`
	Root    string `json:"root"`
	Exclude []Rule `json:"exclude"`
}

// Changed is the sync.changed notification: paths below a watched root that
// changed in the guest. Overflow means the changes were not all seen (too
// many, or the kernel's queue overflowed): the host lists the whole root.
// Error says why the watch is incomplete or ended.
type Changed struct {
	ID       string   `json:"id"`
	Paths    []string `json:"paths,omitempty"`
	Overflow bool     `json:"overflow,omitzero"`
	Error    string   `json:"error,omitempty"`
}

// watcher watches one share's guest tree with inotify, one watch per
// directory that is neither excluded nor reached through a link. Only its
// own goroutine (run) touches its maps once Watch has started it.
type watcher struct {
	id, root string
	rules    Rules
	m        *Manager
	fd, stop int
	dirs     map[int32]string // watch descriptor to directory, relative to root
	wds      map[string]int32
	pending  map[string]bool // path to whether it is a directory
	overflow bool
	errText  string
	done     chan struct{}

	mu     sync.Mutex // guards closed, so that stop is never written once closed
	closed bool
}

// Watch starts watching a share's guest tree, replacing an earlier watch of
// the same id: every change after the answer is reported in sync.changed.
// The root is created if missing, refusing one that is or lies below a link.
func (m *Manager) Watch(p WatchParams) (struct{}, error) {
	if err := errors.Join(checkID(p.ID), checkRoot(p.Root)); err != nil {
		return struct{}{}, err
	}
	rules, err := CompileRules(p.Exclude)
	if err != nil {
		return struct{}{}, err
	}
	m.Unwatch(StatusParams{ID: p.ID})
	r, err := openShareRoot(p.Root, true)
	if err != nil {
		return struct{}{}, proto.FromOS(err)
	}
	r.Close()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return struct{}{}, proto.FromOS(os.NewSyscallError("inotify_init1", err))
	}
	stop, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		unix.Close(fd)
		return struct{}{}, proto.FromOS(os.NewSyscallError("eventfd", err))
	}
	w := &watcher{id: p.ID, root: p.Root, rules: rules, m: m, fd: fd, stop: stop,
		dirs: map[int32]string{}, wds: map[string]int32{}, pending: map[string]bool{}, done: make(chan struct{})}
	if err := w.addTree("", false); err != nil {
		w.close()
		return struct{}{}, proto.FromOS(err)
	}
	m.mu.Lock()
	m.watches[p.ID] = w
	m.mu.Unlock()
	slog.Info("sync watching", "id", p.ID, "directories", len(w.dirs))
	go w.run()
	return struct{}{}, nil
}

// Unwatch stops watching a share; one not watched is no failure.
func (m *Manager) Unwatch(p StatusParams) (struct{}, error) {
	if err := checkID(p.ID); err != nil {
		return struct{}{}, err
	}
	m.mu.Lock()
	w := m.watches[p.ID]
	delete(m.watches, p.ID)
	m.mu.Unlock()
	if w != nil {
		w.mu.Lock()
		if !w.closed {
			unix.Write(w.stop, []byte{1, 0, 0, 0, 0, 0, 0, 0})
		}
		w.mu.Unlock()
		<-w.done
	}
	return struct{}{}, nil
}

func (w *watcher) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	unix.Close(w.fd)
	unix.Close(w.stop)
}

// ended forgets the watch once its goroutine returns on its own.
func (w *watcher) ended() {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	if w.m.watches[w.id] == w {
		delete(w.m.watches, w.id)
	}
}

// addTree watches rel and every directory below it that is not excluded,
// listing them through the root so that no link is followed. With report,
// what it finds is pending too: a directory made or moved in before its
// watch existed.
func (w *watcher) addTree(rel string, report bool) error {
	r, err := openShareRoot(w.root, false)
	if err != nil {
		return err
	}
	defer r.Close()
	var walk func(rel string) error
	walk = func(rel string) error {
		// A directory on the way may be swapped for a link before the watch
		// is added: its events then name paths that sync.manifest and
		// sync.read refuse to follow, so it costs a listing, nothing more.
		wd, err := unix.InotifyAddWatch(w.fd, join(w.root, rel), watchMask)
		if errors.Is(err, unix.ENOSPC) {
			return errors.New("the inotify watch limit (fs.inotify.max_user_watches) is reached")
		}
		if err != nil {
			return nil // gone, or no longer a directory: its parent's events say so
		}
		w.dirs[int32(wd)] = rel
		w.wds[rel] = int32(wd)
		entries, err := listDir(r, rel)
		if err != nil {
			return nil
		}
		for _, de := range entries {
			path := joinRel(rel, de.name)
			if de.kind == kindOther || w.skip(path, de.kind == kindDir) {
				continue
			}
			if report {
				w.pending[path] = de.kind == kindDir
			}
			if de.kind == kindDir {
				if err := walk(path); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(rel)
}

// skip reports a path the host never syncs: a sync.apply temporary file, a
// name the host cannot hold, or an excluded one.
func (w *watcher) skip(path string, dir bool) bool {
	_, name := splitRel(path)
	if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".glov-sync") {
		return true
	}
	return checkRel(path) != nil || excludedAbove(w.rules, path) || w.rules.Excluded(path, dir)
}

// forget drops the watches of rel and the directories below it (moved away).
func (w *watcher) forget(rel string) {
	for path, wd := range w.wds {
		if inScope(path, rel) {
			unix.InotifyRmWatch(w.fd, uint32(wd))
			delete(w.wds, path)
			delete(w.dirs, wd)
		}
	}
}

func (w *watcher) run() {
	defer close(w.done)
	defer w.ended()
	defer w.close()
	buf := make([]byte, 64<<10)
	var first, last time.Time
	for {
		timeout := -1
		if !first.IsZero() {
			due := last.Add(settle)
			if limit := first.Add(maxDelay); limit.Before(due) {
				due = limit
			}
			timeout = max(0, int(time.Until(due).Milliseconds()))
		}
		fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}, {Fd: int32(w.stop), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, timeout); err != nil && !errors.Is(err, unix.EINTR) {
			w.fail(fmt.Sprintf("poll: %v", err))
			return
		}
		if fds[1].Revents != 0 {
			return
		}
		if fds[0].Revents != 0 {
			n, err := unix.Read(w.fd, buf)
			if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
				w.fail(fmt.Sprintf("read inotify events: %v", err))
				return
			}
			if n > 0 && w.events(buf[:n]) {
				w.flush()
				return // the root went away
			}
			if len(w.pending) > 0 || w.overflow {
				last = time.Now()
				if first.IsZero() {
					first = last
				}
			}
			continue
		}
		if !first.IsZero() {
			w.flush()
			first = time.Time{}
		}
	}
}

// events takes in one read's events; it reports whether the root itself was
// deleted or moved, which ends the watch.
func (w *watcher) events(buf []byte) (rootGone bool) {
	for len(buf) >= unix.SizeofInotifyEvent {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[0]))
		end := unix.SizeofInotifyEvent + int(ev.Len)
		if end > len(buf) {
			break
		}
		name := strings.TrimRight(string(buf[unix.SizeofInotifyEvent:end]), "\x00")
		buf = buf[end:]
		if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
			w.overflow = true
			continue
		}
		dir, ok := w.dirs[ev.Wd]
		if !ok {
			continue
		}
		if ev.Mask&unix.IN_IGNORED != 0 {
			delete(w.dirs, ev.Wd)
			if w.wds[dir] == ev.Wd {
				delete(w.wds, dir)
			}
			continue
		}
		if ev.Mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
			if dir == "" {
				w.overflow, w.errText = true, "the share's guest folder was deleted or moved: watch it again"
				return true
			}
			continue // its parent reports it
		}
		if name == "" {
			continue // the directory's own attributes: nothing the host syncs
		}
		path := joinRel(dir, name)
		isDir := ev.Mask&unix.IN_ISDIR != 0
		if w.skip(path, isDir) {
			continue
		}
		w.pending[path] = isDir
		switch {
		case isDir && ev.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0:
			if err := w.addTree(path, true); err != nil && w.errText == "" {
				w.overflow, w.errText = true, err.Error()
			}
		case isDir && ev.Mask&unix.IN_MOVED_FROM != 0:
			w.forget(path)
		}
	}
	return false
}

// flush reports what is pending, leaving out the files that are as the
// share's records say: the Guest Service's own sync.apply writes and
// deletes. Directories are always reported: they have no records, and a
// directory moved or deleted takes its files along unreported.
func (w *watcher) flush() {
	paths := w.echoless()
	clear(w.pending)
	if !w.overflow && len(paths) == 0 {
		return
	}
	c := Changed{ID: w.id, Error: w.errText, Overflow: w.overflow || len(paths) > maxChanged}
	if !c.Overflow {
		c.Paths = paths
	}
	w.overflow, w.errText = false, ""
	w.m.notify("sync.changed", c)
}

func (w *watcher) echoless() []string {
	s := w.m.session(w.id)
	s.busy <- struct{}{} // briefly: a running call holds it, then this
	defer s.unlock()
	paths := make([]string, 0, len(w.pending))
	for path, isDir := range w.pending {
		if !isDir && s.loaded && samePath(s.root, w.root) {
			r, known := s.records[path]
			k, err := fileKey(join(w.root, path))
			if known && err == nil && k == r.Key || !known && errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

func (w *watcher) fail(text string) {
	slog.Error("sync watch ended", "id", w.id, "err", text)
	w.m.notify("sync.changed", Changed{ID: w.id, Overflow: true, Error: "the guest stopped watching: " + text})
}
