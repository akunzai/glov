package live

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const notifyFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION

// HostWatcher reports changes below a host folder with ReadDirectoryChangesW:
// paths relative to it, '/'-separated, or an overflow when the system
// dropped changes (its buffer filled).
type HostWatcher struct {
	h, stop windows.Handle
	// ov is on the heap, which does not move: the system writes to it
	// while the call is pending.
	ov      windows.Overlapped
	once    sync.Once
	stopped chan struct{}
}

// WatchHost watches dir and everything below it until Close, sending each
// batch of changed paths to changed; overflow is called when changes were
// lost. A folder that is a junction or link is refused by the caller first
// (share.Canonical); the watch itself does not follow those below it:
// changes behind a junction are not reported.
func WatchHost(dir string, changed func(paths []string), overflow func()) (*HostWatcher, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s to watch it: %w", dir, err)
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	w := &HostWatcher{h: h, stop: stop, stopped: make(chan struct{})}
	go w.run(changed, overflow)
	return w, nil
}

func (w *HostWatcher) run(changed func([]string), overflow func()) {
	defer close(w.stopped)
	defer windows.CloseHandle(w.h)
	done, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		overflow()
		return
	}
	defer windows.CloseHandle(done)
	// Kept alive across the asynchronous call: the system writes into it.
	buf := make([]byte, 64<<10)
	for {
		ov := &w.ov
		*ov = windows.Overlapped{HEvent: done}
		windows.ResetEvent(done)
		var n uint32
		err := windows.ReadDirectoryChanges(w.h, &buf[0], uint32(len(buf)), true, notifyFilter, nil, ov, 0)
		if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
			overflow()
			return
		}
		ev, _ := windows.WaitForMultipleObjects([]windows.Handle{done, w.stop}, false, windows.INFINITE)
		if ev != windows.WAIT_OBJECT_0 {
			windows.CancelIoEx(w.h, ov)
			windows.GetOverlappedResult(w.h, ov, &n, true)
			return
		}
		if err := windows.GetOverlappedResult(w.h, ov, &n, false); err != nil {
			if errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) {
				overflow()
				continue
			}
			overflow()
			return
		}
		if n == 0 {
			overflow()
			continue
		}
		var paths []string
		for off := uint32(0); ; {
			info := (*windows.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
			name := windows.UTF16ToString(unsafe.Slice(&info.FileName, info.FileNameLength/2))
			rel := filepath.ToSlash(name)
			if _, base := splitLast(rel); !TempFile(base) {
				paths = append(paths, rel)
			}
			if info.NextEntryOffset == 0 {
				break
			}
			off += info.NextEntryOffset
		}
		if len(paths) > 0 {
			changed(paths)
		}
	}
}

func splitLast(p string) (string, string) {
	i := strings.LastIndexByte(p, '/')
	return p[:i+1], p[i+1:]
}

// Close stops the watch.
func (w *HostWatcher) Close() {
	w.once.Do(func() { windows.SetEvent(w.stop) })
	<-w.stopped
	windows.CloseHandle(w.stop)
}
