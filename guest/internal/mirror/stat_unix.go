//go:build unix

package mirror

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type kind int

const (
	kindOther kind = iota // symbolic links, devices, sockets: never touched
	kindFile
	kindDir
)

type dirEntry struct {
	name  string
	kind  kind
	size  int64
	mtime int64 // Unix nanoseconds
	key   string
}

func keyOf(st *unix.Stat_t) string {
	return fmt.Sprintf("%d:%d:%d:%d", st.Size, unix.TimespecToNsec(st.Mtim), unix.TimespecToNsec(st.Ctim), st.Ino)
}

func kindOf(mode uint32) kind {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return kindFile
	case unix.S_IFDIR:
		return kindDir
	}
	return kindOther
}

// listDir lists rel below r with each entry's stat key: size, mtime, ctime
// and inode. The directory is opened through r, and the entries are stat'ed
// relative to its handle, so a parent swapped for a link cannot redirect it.
func listDir(r *os.Root, rel string) ([]dirEntry, error) {
	f, err := r.Open(native(rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]dirEntry, 0, len(des))
	for _, de := range des {
		var st unix.Stat_t
		if err := unix.Fstatat(int(f.Fd()), de.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue // removed while listing
		}
		out = append(out, dirEntry{de.Name(), kindOf(uint32(st.Mode)), st.Size, unix.TimespecToNsec(st.Mtim), keyOf(&st)})
	}
	return out, nil
}

// fileKey is the stat key listDir would report for path.
func fileKey(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return "", &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return keyOf(&st), nil
}

// entryAt stats rel below r without following it if it is a link; the
// directory holding it is opened through r.
func entryAt(r *os.Root, rel string) (dirEntry, error) {
	dir, name := splitRel(rel)
	f, err := r.Open(native(dir))
	if err != nil {
		return dirEntry{}, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(int(f.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return dirEntry{}, &os.PathError{Op: "lstat", Path: rel, Err: err}
	}
	return dirEntry{name, kindOf(uint32(st.Mode)), st.Size, unix.TimespecToNsec(st.Mtim), keyOf(&st)}, nil
}
