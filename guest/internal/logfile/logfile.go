// Package logfile is a size-capped log file: when the next write would pass
// the cap, the file is renamed to <path>.1 (replacing the previous one) and a
// fresh file starts, so at most about twice the cap stays on disk.
package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Writer struct {
	path string
	max  int64
	mu   sync.Mutex
	f    *os.File
	size int64
}

func Open(path string, max int64) (*Writer, error) {
	if err := PrepareDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	w := &Writer{path: path, max: max}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// refuseLink fails if path exists and is not a plain file, so a planted
// symlink or junction is never followed (the Guest Service may run as LocalSystem).
func refuseLink(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (%v)", path, fi.Mode().Type())
	}
	return nil
}

func (w *Writer) open() error {
	if err := refuseLink(w.path); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, fi.Size()
	return nil
}

func (w *Writer) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size > 0 && w.size+int64(len(b)) > w.max {
		w.f.Close()
		if err := refuseLink(w.path); err != nil {
			return 0, err
		}
		os.Rename(w.path, w.path+".1")
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(b)
	w.size += int64(n)
	return n, err
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
