package live

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/akunzai/glov/windows/internal/share"
	"golang.org/x/sys/windows"
)

// Host is the host side of a live share: the folder, read and written as
// the user through an os.Root, which refuses any path that leaves it through
// a link or junction. Links, junctions, special files and names that are
// not valid Unicode are Other: never read, written or deleted. A path from
// the guest that checkName refuses, or that is excluded, never reaches the
// file system.
type Host struct {
	Root *os.Root
	Ex   share.Excludes
	// Keys caches each file's hash by its stat key (size, last write and
	// change time, volume and file ID), so unchanged files are not hashed.
	Keys map[string]HostKey
}

// NewHost serves root with excludes ex and the hash cache keys. The host
// matches ex in any case, as Windows finds names: a folder SECRET that a
// guest made is where a program on Windows writes secret	oken, so an
// exclude of secret/ keeps both out.
func NewHost(root *os.Root, ex share.Excludes, keys map[string]HostKey) *Host {
	if keys == nil {
		keys = map[string]HostKey{}
	}
	return &Host{Root: root, Ex: ex.IgnoringCase(), Keys: keys}
}

func (h *Host) Name() string { return "host" }

// TempFile reports a name Put and the guest's sync.apply write to first.
func TempFile(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".glov-sync")
}

func native(p string) string {
	if p == "" {
		return "."
	}
	return filepath.FromSlash(p)
}

func (h *Host) List(scope string) (Snapshot, error) {
	out := Snapshot{}
	var walk func(rel string) error
	walk = func(rel string) error {
		d, err := h.Root.Open(native(rel))
		if err != nil {
			return err
		}
		entries, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := path.Join(rel, e.Name())
			if TempFile(e.Name()) {
				continue
			}
			st, ok := h.state(p, e.Type())
			if !ok {
				continue
			}
			out[p] = st
			if st.Dir {
				if err := walk(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(scope); err != nil && !(scope != "" && errors.Is(err, fs.ErrNotExist)) {
		return nil, err
	}
	return out, nil
}

// state is p's state, from its type as a listing reported it; false when
// excluded or gone.
func (h *Host) state(p string, typ fs.FileMode) (State, bool) {
	isDir := typ.IsDir()
	if h.Ex.Excluded(p, isDir) {
		return State{}, false
	}
	_, name := path.Split(p)
	if !utf8.ValidString(name) || !isDir && !typ.IsRegular() {
		return State{Other: true}, true
	}
	fi, err := h.Root.Lstat(native(p))
	if err != nil {
		return State{}, false
	}
	switch {
	case fi.IsDir():
		return State{Dir: true}, true
	case !fi.Mode().IsRegular():
		return State{Other: true}, true
	}
	st, err := h.file(p)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false
	}
	if err != nil {
		return State{Other: true}, true // held by a program that shares no access
	}
	return st, true
}

// file hashes the regular file p, or takes its hash from the cache.
func (h *Host) file(p string) (State, error) {
	f, err := share.OpenFile(h.Root, p)
	if err != nil {
		return State{}, err
	}
	defer f.Close()
	key, mtime, err := statKey(f)
	if err != nil {
		return State{}, err
	}
	if k, ok := h.Keys[p]; ok && k.Key == key {
		return State{Hash: k.Hash, Mtime: mtime}, nil
	}
	s := sha256.New()
	if _, err := io.CopyBuffer(s, f, make([]byte, 1<<20)); err != nil {
		return State{}, err
	}
	sum := hex.EncodeToString(s.Sum(nil))
	h.Keys[p] = HostKey{key, sum}
	return State{Hash: sum, Mtime: mtime}, nil
}

func (h *Host) Stat(paths []string) (Snapshot, error) {
	out := Snapshot{}
	for _, p := range paths {
		if st, ok := h.stat(p); ok {
			out[p] = st
		}
	}
	return out, nil
}

// stat is p's state when every directory above it is a plain directory
// (no link or junction), as a listing reaches it; a name checkName refuses
// is absent.
func (h *Host) stat(p string) (State, bool) {
	if h.refuse(p, false) != nil {
		return State{}, false
	}
	parts := strings.Split(p, "/")
	for i := range parts[:len(parts)-1] {
		dir := strings.Join(parts[:i+1], "/")
		fi, err := h.Root.Lstat(native(dir))
		if err != nil || !fi.IsDir() || h.Ex.Excluded(dir, true) {
			return State{}, false
		}
	}
	fi, err := h.Root.Lstat(native(p))
	if err != nil {
		return State{}, false
	}
	return h.state(p, fi.Mode().Type())
}

func (h *Host) Open(p string) (io.ReadCloser, error) {
	if err := h.refuse(p, false); err != nil {
		return nil, err
	}
	return share.OpenFile(h.Root, p)
}

// current is what p holds as a Put or Remove's expect would say: its hash,
// "" for nothing, or "dir"/"other".
func (h *Host) current(p string) (string, error) {
	fi, err := h.Root.Lstat(native(p))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case fi.IsDir():
		return "dir", nil
	case !fi.Mode().IsRegular():
		return "other", nil
	}
	st, err := h.file(p)
	return st.Hash, err
}

// refuse is why the host neither reads nor changes p for the guest (a
// directory when dir, unless p exists): a name checkName refuses (ErrName),
// or p or a directory above it excluded (ErrExcluded). A guest honours the
// excludes itself, but the guest is assumed hostile. NTFS finds a name in
// any case and by its 8.3 short name, while the excludes match names as
// the folder spells them, so the check takes each existing part of p by
// its name on disk, and the type p has there.
func (h *Host) refuse(p string, dir bool) error {
	if err := checkName(p); err != nil {
		return err
	}
	real, fi, err := h.onDisk(p)
	if err != nil {
		return err
	}
	if fi != nil {
		dir = fi.IsDir()
	}
	if h.Ex.ExcludedOrParent(p, dir) || h.Ex.ExcludedOrParent(real, dir) {
		return fmt.Errorf("%q: %w", p, ErrExcluded)
	}
	return nil
}

// onDisk is p with each part that exists spelled as on disk (the rest as
// given), and p's own Lstat when it exists. A part above p that exists and
// is not a plain directory (a file, link or junction) is ErrConflict: no
// listing reaches below it.
func (h *Host) onDisk(p string) (string, fs.FileInfo, error) {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		at := strings.Join(append(parts[:i:i], seg), "/")
		fi, err := h.Root.Lstat(native(at))
		if errors.Is(err, fs.ErrNotExist) {
			return strings.Join(parts, "/"), nil, nil
		}
		if err != nil {
			return "", nil, err
		}
		var data windows.Win32finddata
		name, err := windows.UTF16PtrFromString(filepath.Join(h.Root.Name(), native(at)))
		if err != nil {
			return "", nil, err
		}
		hd, err := windows.FindFirstFile(name, &data)
		if err != nil {
			return "", nil, err
		}
		windows.FindClose(hd)
		parts[i] = windows.UTF16ToString(data.FileName[:])
		if i == len(parts)-1 {
			return strings.Join(parts, "/"), fi, nil
		}
		if !fi.IsDir() {
			return "", nil, fmt.Errorf("%q: %w", p, ErrConflict)
		}
	}
	return p, nil, nil
}

// Put, Remove, Mkdir and Rmdir take paths the guest names, so each refuses
// one refuse refuses before its first file system call.
func (h *Host) Put(p string, r io.Reader, want, expect string) error {
	if err := h.refuse(p, false); err != nil {
		return err
	}
	dir, name := path.Split(p)
	if dir != "" {
		if err := h.Root.MkdirAll(native(strings.TrimSuffix(dir, "/")), 0o755); err != nil {
			return err
		}
	}
	tmp := path.Join(dir, "."+name+"."+rand.Text()[:12]+".glov-sync")
	f, err := h.Root.OpenFile(native(tmp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	s := sha256.New()
	_, err = io.CopyBuffer(io.MultiWriter(f, s), r, make([]byte, 1<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(s.Sum(nil)) != want {
		err = ErrChanged
	}
	if err == nil {
		err = h.expect(p, expect)
	}
	if err == nil {
		err = h.Root.Rename(native(tmp), native(p))
	}
	if err != nil {
		h.Root.Remove(native(tmp))
		return err
	}
	if f, err := h.Root.Open(native(p)); err == nil {
		if key, _, err := statKey(f); err == nil {
			h.Keys[p] = HostKey{key, want}
		}
		f.Close()
	}
	return nil
}

func (h *Host) expect(p, expect string) error {
	got, err := h.current(p)
	if err != nil {
		return err
	}
	if got != expect {
		return ErrConflict
	}
	return nil
}

func (h *Host) Remove(p, expect string) error {
	if err := h.refuse(p, false); err != nil {
		return err
	}
	if err := h.expect(p, expect); err != nil {
		return err
	}
	if err := h.Root.Remove(native(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	delete(h.Keys, p)
	return nil
}

func (h *Host) Mkdir(p string) error {
	if err := h.refuse(p, true); err != nil {
		return err
	}
	return h.Root.MkdirAll(native(p), 0o755)
}

func (h *Host) Rmdir(p string) error {
	if err := h.refuse(p, true); err != nil {
		return err
	}
	if fi, err := h.Root.Lstat(native(p)); err == nil && !fi.IsDir() {
		return ErrConflict // the guest saw a directory; the host holds something else
	}
	err := h.Root.Remove(native(p))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if d, oerr := h.Root.Open(native(p)); oerr == nil {
		entries, _ := d.ReadDir(1)
		d.Close()
		if len(entries) > 0 {
			return nil // holds excluded or skipped files
		}
	}
	return err
}

func (h *Host) Flush() map[string]error { return nil }

// statKey is an open file's stat key and last write time (Unix ns).
func statKey(f *os.File) (string, int64, error) {
	hd := windows.Handle(f.Fd())
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(hd, &fi); err != nil {
		return "", 0, err
	}
	var basic struct {
		CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
		FileAttributes                                          uint32
		_                                                       uint32
	}
	if err := windows.GetFileInformationByHandleEx(hd, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return "", 0, err
	}
	size := int64(fi.FileSizeHigh)<<32 | int64(fi.FileSizeLow)
	id := uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)
	key := fmt.Sprintf("%d:%d:%d:%x:%x", size, basic.LastWriteTime, basic.ChangeTime, fi.VolumeSerialNumber, id)
	return key, (basic.LastWriteTime - 116444736000000000) * 100, nil
}
