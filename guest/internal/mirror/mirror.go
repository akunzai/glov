// Package mirror implements sync.manifest and sync.apply: the guest side of
// mirroring a host folder into a guest directory (docs/guest-protocol.md,
// "Share sync"). The host decides what to write and delete; this package
// lists and hashes the guest tree, keeps the guest manifest (each file's stat
// key and SHA-256) outside it, and applies the host's operations.
package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akunzai/glov/guest/proto"
)

// pageBytes bounds one sync.manifest result, well under the 1 MiB frame. It is
// small because one large reply is what went missing after hours of syncs
// under Mac memory pressure while small ones kept going (cause unconfirmed).
const pageBytes = 128 << 10

// Manager holds the guest manifests under dir, one file per share id.
type Manager struct {
	dir     string
	prepare func(dir string) error

	mu       sync.Mutex
	sessions map[string]*session
	watches  map[string]*watcher // Linux: sync.watch
	notify   func(method string, params any)
}

// SetNotify sets how sync.changed reaches the host.
func (m *Manager) SetNotify(notify func(method string, params any)) { m.notify = notify }

// NewManager keeps manifests in dir; prepare creates it before the first save
// (on Windows with a DACL that keeps non-administrators out).
func NewManager(dir string, prepare func(dir string) error) *Manager {
	return &Manager{dir: dir, prepare: prepare, sessions: map[string]*session{}, watches: map[string]*watcher{},
		notify: func(string, any) {}}
}

type record struct {
	Key  string `json:"k"`
	Hash string `json:"h"`
}

type stateFile struct {
	Root  string            `json:"root"`
	Files map[string]record `json:"files"`
}

// session is one share's state between calls: the manifest as loaded and
// updated, the last walk (for paging) and the writes in progress. busy is its
// lock (see lock); progress and cancel are read and set without it.
//
// Invariant: while pending holds a write, dir is open, so its temporary file
// can still be removed through it; only end drops writes or closes dir.
type session struct {
	busy     chan struct{}
	progress progress
	cancel   atomic.Bool

	root    string
	loaded  bool
	fresh   bool
	records map[string]record
	walk    []Entry
	pending map[string]*pendingWrite
	failed  map[string]bool // writes that failed earlier in this sync: their later chunks are dropped
	dir     *os.Root        // root, for every change below it (openRoot)
}

func (m *Manager) session(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		s = &session{busy: make(chan struct{}, 1)}
		m.sessions[id] = s
	}
	return s
}

func (m *Manager) statePath(id string) string { return filepath.Join(m.dir, id+".json") }

// load reads the manifest of id, discarding it when it was recorded for
// another root, and drops whatever an unfinished sync left.
func (m *Manager) load(s *session, id, root string) {
	s.end(false)
	s.root, s.loaded, s.fresh, s.records, s.walk = root, true, true, map[string]record{}, nil
	s.failed = map[string]bool{}
	data, err := os.ReadFile(m.statePath(id))
	if err != nil {
		return
	}
	var st stateFile
	if json.Unmarshal(data, &st) != nil || !samePath(st.Root, root) {
		return
	}
	s.fresh = false
	if st.Files != nil {
		s.records = st.Files
	}
}

func (m *Manager) save(s *session, id string) error {
	if err := m.prepare(m.dir); err != nil {
		return err
	}
	data, err := json.Marshal(stateFile{s.root, s.records})
	if err != nil {
		return err
	}
	tmp := m.statePath(id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.statePath(id))
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// ManifestParams are sync.manifest's params.
type ManifestParams struct {
	ID      string `json:"id"`
	Root    string `json:"root"`
	Subdir  string `json:"subdir"`
	Exclude []Rule `json:"exclude"`
	Verify  bool   `json:"verify"`
	Offset  int    `json:"offset"`
	// Paths, if set, lists just these entries (at most maxPaths, none
	// descended into) in place of a walk: what a live share's host asks
	// after sync.changed.
	Paths []string `json:"paths,omitempty"`
	// Owner is checked (a known guest user, Linux only) and echoed, so the
	// host learns before any write whether sync.apply will honour it.
	Owner string `json:"owner,omitempty"`
}

// Entry is one file or directory below the root.
type Entry struct {
	Path     string `json:"path"`
	Dir      bool   `json:"dir,omitzero"`
	Size     int64  `json:"size"`
	Mtime    int64  `json:"mtime,omitzero"` // a file's, in Unix nanoseconds
	Hash     string `json:"hash"`
	Modified bool   `json:"modified,omitzero"`
	Error    string `json:"error,omitempty"`
}

type ManifestResult struct {
	Entries []Entry `json:"entries"`
	Total   int     `json:"total"`
	Hashed  int     `json:"hashed"`
	Fresh   bool    `json:"fresh"`
	Owner   string  `json:"owner,omitempty"`
	// Missing are the Paths that are no file or directory a listing would
	// reach: absent, below a missing or linked directory, or a link. A path
	// in neither Entries nor Missing is excluded.
	Missing []string `json:"missing,omitempty"`
}

// maxPaths bounds ManifestParams.Paths, keeping the answer within a page.
const maxPaths = 512

// Manifest lists root/subdir (offset 0, which also hashes files whose stat key
// changed) or returns the next page of the last listing.
func (m *Manager) Manifest(p ManifestParams) (ManifestResult, error) {
	if p.Owner != "" {
		if _, err := lookupOwner(p.Owner); err != nil {
			return ManifestResult{}, err
		}
	}
	res, err := m.manifest(p)
	if err == nil {
		res.Owner = p.Owner
	}
	return res, err
}

func (m *Manager) manifest(p ManifestParams) (ManifestResult, error) {
	if err := errors.Join(checkID(p.ID), checkRoot(p.Root), checkRel(p.Subdir)); err != nil {
		return ManifestResult{}, err
	}
	rules, err := CompileRules(p.Exclude)
	if err != nil {
		return ManifestResult{}, err
	}
	if len(p.Paths) > 0 {
		return m.manifestPaths(p, rules)
	}
	s := m.session(p.ID)
	if err := s.lock(p.ID); err != nil {
		return ManifestResult{}, err
	}
	defer s.unlock()
	hashed := 0
	if p.Offset == 0 {
		s.cancel.Store(false)
		s.progress.begin("manifest")
		defer s.progress.end()
		m.load(s, p.ID, p.Root)
		if hashed, err = s.list(p.Subdir, rules, p.Verify); err != nil {
			if errors.Is(err, errCancelled) {
				return ManifestResult{}, proto.Errorf(proto.CodeOS, "the listing of share %s was cancelled", p.ID)
			}
			return ManifestResult{}, proto.FromOS(err)
		}
		slog.Info("sync listed", "id", p.ID, "entries", len(s.walk), "hashed", hashed,
			"bytes", s.progress.bytes.Load(), "seconds", time.Since(s.progress.started).Seconds())
	} else if s.walk == nil || !samePath(s.root, p.Root) || p.Offset < 0 || p.Offset > len(s.walk) {
		return ManifestResult{}, proto.Errorf(proto.CodeInvalidParams, "no listing to continue at offset %d: start with offset 0", p.Offset)
	}
	res := ManifestResult{Entries: []Entry{}, Total: len(s.walk), Hashed: hashed, Fresh: s.fresh}
	size := 0
	for _, e := range s.walk[p.Offset:] {
		size += len(e.Path) + len(e.Hash) + len(e.Error) + 80
		if size > pageBytes && len(res.Entries) > 0 {
			break
		}
		res.Entries = append(res.Entries, e)
	}
	return res, nil
}

// list walks root/scope into s.walk and replaces the records below scope with
// what it found. It returns how many files it hashed.
func (s *session) list(scope string, rules Rules, verify bool) (int, error) {
	prev := s.records
	next := make(map[string]record, len(prev))
	for k, r := range prev {
		if !inScope(k, scope) {
			next[k] = r
		}
	}
	s.walk = []Entry{}
	hashed := 0
	// Everything is read through the root, opened without following links
	// (see openShareRoot), so a link swapped in during the walk cannot make
	// the Guest Service list or hash files outside it.
	dir, err := openShareRoot(s.root, false)
	if err != nil {
		s.records = next
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil // nothing synced yet
		}
		return 0, err
	}
	defer dir.Close()
	cancelled := false
	var walk func(rel string)
	walk = func(rel string) {
		entries, err := listDir(dir, rel)
		if err != nil {
			return // missing (nothing to list) or unreadable (nothing the host may delete)
		}
		for _, de := range entries {
			if cancelled = cancelled || s.cancelled(); cancelled {
				return
			}
			path := joinRel(rel, de.name)
			if de.kind == kindOther || checkRel(path) != nil || rules.Excluded(path, de.kind == kindDir) {
				continue
			}
			if de.kind == kindDir {
				s.walk = append(s.walk, Entry{Path: path, Dir: true})
				walk(path)
				continue
			}
			e := Entry{Path: path, Size: de.size, Mtime: de.mtime}
			r, known := prev[path]
			if known && r.Key == de.key && !verify {
				e.Hash = r.Hash
			} else {
				hashed++
				h, err := s.hashFile(dir, path)
				if errors.Is(err, errCancelled) {
					cancelled = true
					return
				}
				if err != nil {
					e.Error = osText(err)
				}
				e.Hash = h
			}
			e.Modified = !s.fresh && e.Error == "" && (!known || r.Hash != e.Hash)
			if e.Error == "" {
				next[path] = record{de.key, e.Hash}
			}
			s.walk = append(s.walk, e)
		}
	}
	if top, err := dir.Lstat(native(scope)); err == nil && top.IsDir() && top.Mode()&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
		walk(scope)
	}
	if cancelled {
		s.records, s.walk = prev, nil // a partial walk must not drop records it never reached
		return 0, errCancelled
	}
	s.records = next
	return hashed, nil
}

// openForHash opens a file to hash; tests replace it to simulate a read that
// never returns.
var openForHash = func(r *os.Root, rel string) (io.ReadCloser, error) { return r.Open(filepath.FromSlash(rel)) }

// hashFile hashes rel, counting its bytes in the session's progress, and logs
// one that takes long: the file sync.status names when a listing stalls.
func (s *session) hashFile(r *os.Root, rel string) (string, error) {
	s.progress.at(rel)
	s.progress.files.Add(1)
	started := time.Now()
	defer func() {
		if d := time.Since(started); d >= slowHash {
			slog.Warn("slow hash", "path", rel, "seconds", d.Seconds())
		}
	}()
	f, err := openForHash(r, rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, progressReader{f, s}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// osText is the OS's own message, without the path the host already knows.
func osText(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err.Error()
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return le.Err.Error()
	}
	return err.Error()
}

// manifestPaths answers a manifest of p.Paths: each one listed (and hashed
// when its stat key changed) or missing, its record updated.
func (m *Manager) manifestPaths(p ManifestParams, rules Rules) (ManifestResult, error) {
	if len(p.Paths) > maxPaths {
		return ManifestResult{}, proto.Errorf(proto.CodeInvalidParams, "at most %d paths, got %d", maxPaths, len(p.Paths))
	}
	for _, path := range p.Paths {
		if path == "" || checkRel(path) != nil {
			return ManifestResult{}, proto.Errorf(proto.CodeInvalidParams, "invalid path %q", path)
		}
	}
	s := m.session(p.ID)
	if err := s.lock(p.ID); err != nil {
		return ManifestResult{}, err
	}
	defer s.unlock()
	s.cancel.Store(false)
	s.progress.begin("manifest")
	defer s.progress.end()
	if !s.loaded || !samePath(s.root, p.Root) {
		m.load(s, p.ID, p.Root)
	}
	res := ManifestResult{Entries: []Entry{}, Fresh: s.fresh}
	dir, err := openShareRoot(s.root, false)
	if errors.Is(err, fs.ErrNotExist) {
		res.Missing = p.Paths
		return res, nil
	}
	if err != nil {
		return ManifestResult{}, proto.FromOS(err)
	}
	defer dir.Close()
	for _, path := range p.Paths {
		if excludedAbove(rules, path) {
			continue
		}
		de, ok, err := reach(dir, path)
		if err == nil && ok && rules.Excluded(path, de.kind == kindDir) {
			continue
		}
		if err == nil && !ok {
			// Kind unknown: excluded either way is safer than a deletion.
			if rules.Excluded(path, false) || rules.Excluded(path, true) {
				continue
			}
			delete(s.records, path)
			res.Missing = append(res.Missing, path)
			continue
		}
		e := Entry{Path: path, Dir: de.kind == kindDir, Size: de.size, Mtime: de.mtime}
		switch {
		case err != nil:
			e.Error = osText(err)
		case e.Dir:
			e.Size, e.Mtime = 0, 0
		default:
			r, known := s.records[path]
			if known && r.Key == de.key {
				e.Hash = r.Hash
			} else if h, err := s.hashFile(dir, path); err != nil {
				e.Error = osText(err)
			} else {
				e.Hash = h
				res.Hashed++
			}
			e.Modified = !s.fresh && e.Error == "" && (!known || r.Hash != e.Hash)
			if e.Error == "" {
				s.records[path] = record{de.key, e.Hash}
			}
		}
		res.Entries = append(res.Entries, e)
	}
	res.Total = len(res.Entries)
	return res, nil
}

// excludedAbove reports whether rules exclude a directory above path, which
// a walk would not descend into.
func excludedAbove(rules Rules, path string) bool {
	dir, _ := splitRel(path)
	at := ""
	for name := range strings.SplitSeq(dir, "/") {
		if dir == "" {
			break
		}
		at = joinRel(at, name)
		if rules.Excluded(at, true) {
			return true
		}
	}
	return false
}
