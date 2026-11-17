package share

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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

	"golang.org/x/sys/windows"
)

// Tree is a share's host folder as a sync sees it. Paths are relative to
// the share root, '/'-separated.
type Tree struct {
	Files map[string]HostFile
	Dirs  map[string]bool
	// Skipped are paths the sync does not mirror and leaves alone in the
	// guest: symbolic links, junctions, other special files, names that are
	// not valid Unicode, and files it could not open (held by a program
	// that shares no access).
	Skipped []string
	// Hashed counts the files this scan hashed; the others' hashes came
	// from the cache.
	Hashed int
}

// ErrCancelled ends a scan whose cancelled func returned true.
var ErrCancelled = errors.New("cancelled")

type cacheRecord struct {
	Key  string `json:"k"`
	Hash string `json:"h"`
}

type cacheFile struct {
	HostPath string                 `json:"hostPath"`
	Files    map[string]cacheRecord `json:"files"`
}

// Scan lists root/scope, hashing only the files whose stat key (size, last
// write time, change time, file ID) differs from cachePath's record, or
// every file with verify, then rewrites the cache: records below scope are
// replaced by what the scan found, others are kept. The change time cannot
// be set back by ordinary tools and an atomic replace changes the file ID,
// so a rewrite that keeps size and write time is still seen.
//
// Every open goes through root (os.Root), which refuses a path that leaves
// the share through a link; links and junctions themselves are skipped, so
// nothing outside the share is read even if a folder is swapped for one
// while the scan runs. cancelled is checked before each entry and while
// hashing; once it is true Scan returns ErrCancelled and saves nothing.
func Scan(root *os.Root, scope string, ex Excludes, cachePath string, verify bool, cancelled func() bool) (Tree, error) {
	hostPath := root.Name()
	var cache cacheFile
	if b, err := os.ReadFile(cachePath); err == nil {
		json.Unmarshal(b, &cache)
	}
	previous := cache.Files
	if cache.HostPath != hostPath {
		previous = nil
	}
	next := map[string]cacheRecord{}
	for p, r := range previous {
		if !inScope(p, scope) {
			next[p] = r
		}
	}
	t := Tree{Files: map[string]HostFile{}, Dirs: map[string]bool{}}
	var walk func(rel string) error
	walk = func(rel string) error {
		d, err := root.Open(dirName(rel))
		if err != nil {
			return err
		}
		entries, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return fmt.Errorf("list %s: %w", filepath.Join(hostPath, filepath.FromSlash(rel)), err)
		}
		for _, e := range entries {
			if cancelled() {
				return ErrCancelled
			}
			p := path.Join(rel, e.Name())
			isDir := e.Type().IsDir()
			if ex.Excluded(p, isDir) {
				continue
			}
			if !utf8.ValidString(e.Name()) || !isDir && !e.Type().IsRegular() {
				t.Skipped = append(t.Skipped, p)
				continue
			}
			if isDir {
				// Looked at again: ReadDir's type may be older than this.
				if fi, err := root.Lstat(filepath.FromSlash(p)); err != nil || !fi.IsDir() {
					t.Skipped = append(t.Skipped, p)
					continue
				}
				t.Dirs[p] = true
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			f, err := root.Open(filepath.FromSlash(p))
			if err != nil {
				t.Skipped = append(t.Skipped, p)
				continue
			}
			size, key, err := statKey(f)
			if err == nil {
				r, ok := previous[p]
				if !ok || r.Key != key || verify {
					r = cacheRecord{Key: key}
					if r.Hash, err = hashFile(f, cancelled); err == nil {
						t.Hashed++
					}
				}
				if err == nil {
					t.Files[p] = HostFile{Size: size, Hash: r.Hash}
					next[p] = r
				}
			}
			f.Close()
			if errors.Is(err, ErrCancelled) {
				return err
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", filepath.Join(hostPath, filepath.FromSlash(p)), err)
			}
		}
		return nil
	}
	if err := walk(scope); err != nil {
		return Tree{}, err
	}
	b, err := json.Marshal(cacheFile{HostPath: hostPath, Files: next})
	if err != nil {
		return Tree{}, err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return Tree{}, err
	}
	if err := os.WriteFile(cachePath+".tmp", b, 0o600); err != nil {
		return Tree{}, err
	}
	return t, os.Rename(cachePath+".tmp", cachePath)
}

func inScope(p, scope string) bool {
	return scope == "" || p == scope || strings.HasPrefix(p, scope+"/")
}

func dirName(rel string) string {
	if rel == "" {
		return "."
	}
	return filepath.FromSlash(rel)
}

// statKey returns an open file's size and stat key.
func statKey(f *os.File) (int64, string, error) {
	h := windows.Handle(f.Fd())
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return 0, "", err
	}
	var basic struct {
		CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
		FileAttributes                                          uint32
		_                                                       uint32
	}
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return 0, "", err
	}
	size := int64(fi.FileSizeHigh)<<32 | int64(fi.FileSizeLow)
	id := uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)
	return size, fmt.Sprintf("%d:%d:%d:%x:%x", size, basic.LastWriteTime, basic.ChangeTime, fi.VolumeSerialNumber, id), nil
}

// hashFile returns the hex SHA-256 of f from its start.
func hashFile(f *os.File, cancelled func() bool) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	s := sha256.New()
	if _, err := io.CopyBuffer(s, cancellable{f, cancelled}, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(s.Sum(nil)), nil
}

type cancellable struct {
	r         io.Reader
	cancelled func() bool
}

func (c cancellable) Read(b []byte) (int, error) {
	if c.cancelled() {
		return 0, ErrCancelled
	}
	return c.r.Read(b)
}

// OpenFile opens the share file rel ('/'-separated) for sending, through
// root, refusing anything but a regular file.
func OpenFile(root *os.Root, rel string) (*os.File, error) {
	p := filepath.FromSlash(rel)
	fi, err := root.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is no longer a regular file", rel)
	}
	f, err := root.Open(p)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is no longer a regular file", rel)
	}
	return f, nil
}

// ScopeDir checks that scope is a plain directory inside root.
func ScopeDir(root *os.Root, scope string) error {
	if scope == "" {
		return nil
	}
	p := ""
	for _, part := range strings.Split(scope, "/") {
		p = filepath.Join(p, part)
		fi, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no directory %s in the share", scope)
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s in the share is not a plain directory (a file, link or junction); nothing was synced", scope)
		}
	}
	return nil
}
