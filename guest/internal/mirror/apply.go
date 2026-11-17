package mirror

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/akunzai/glov/guest/proto"
)

// parallelWrites is how many whole-file writes of one batch run at once:
// creating, writing and renaming a small file costs far more than its bytes.
const parallelWrites = 8

// Op is one sync.apply operation.
type Op struct {
	Op     string `json:"op"` // delete, rmdir, mkdir or write
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
	End    bool   `json:"end"`
	Hash   string `json:"hash"`
	// Expect, if set, is what a write (with its end chunk) or a delete
	// replaces: the SHA-256 the guest file must have, or "" for no file.
	// Anything else fails with Conflict, so a live share never overwrites a
	// guest change the host has not seen.
	Expect *string `json:"expect,omitempty"`
}

type ApplyParams struct {
	ID     string `json:"id"`
	Root   string `json:"root"`
	Ops    []Op   `json:"ops"`
	Commit bool   `json:"commit"`
	// Owner, if set, is a guest user (Linux only) who owns root and every
	// folder and file this call creates or writes, instead of root.
	Owner string `json:"owner,omitempty"`
}

// owner is ApplyParams.Owner resolved.
type owner struct{ uid, gid int }

// Failure is an operation that failed. Locked means another process holds
// the file; Denied means access was refused. Both are set when Windows cannot
// tell them apart: replacing or deleting a file held without
// FILE_SHARE_DELETE fails with access denied, as a read-only file does.
type Failure struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Locked  bool   `json:"locked"`
	Denied  bool   `json:"denied"`
	// Conflict: the guest file is not what Expect said; nothing changed.
	Conflict bool `json:"conflict,omitzero"`
}

type ApplyResult struct {
	Failures []Failure `json:"failures"`
	// Owner echoes ApplyParams.Owner once applied, so the host can tell a
	// Guest Service that ignored it.
	Owner string `json:"owner,omitempty"`
}

type pendingWrite struct {
	f      *os.File
	tmp    string // relative to the root
	offset int64
	sum    hash.Hash
}

// end finishes a call. Unless keep, it drops the writes in progress, removing
// their temporary files through the root that is still open; then it closes
// the root once no write spans calls, since on Windows an open handle stops
// anyone renaming or deleting the share folder. Every way a call ends goes
// through here, which keeps the session's invariant (see session).
func (s *session) end(keep bool) {
	if !keep {
		for _, w := range s.pending {
			w.f.Close()
			s.dir.Remove(w.tmp)
		}
		s.pending = map[string]*pendingWrite{}
	}
	if len(s.pending) == 0 && s.dir != nil {
		s.dir.Close()
		s.dir = nil
	}
}

// openRoot opens the share's guest root, creating it first, and refuses one
// that is or lies below a link (see openShareRoot). Every change below it goes
// through this os.Root, which refuses any path that would leave the root
// through a symbolic link or junction, checked at each operation: a guest
// process that swaps a directory for a link cannot redirect the Guest Service's
// (LocalSystem's) writes, renames or deletes outside the share. It is opened
// afresh on every call, so a root replaced by a link between calls is caught.
// The previous one stays open until the new one is: if the new one cannot be
// opened, the caller's end still removes the temporary files through it.
func (s *session) openRoot() error {
	r, err := openShareRoot(s.root, true)
	if err != nil {
		return err
	}
	if s.dir != nil {
		s.dir.Close()
	}
	s.dir = r
	return nil
}

// Apply runs the host's operations in order, collecting the ones that fail.
func (m *Manager) Apply(p ApplyParams) (ApplyResult, error) {
	if err := errors.Join(checkID(p.ID), checkRoot(p.Root)); err != nil {
		return ApplyResult{}, err
	}
	for _, op := range p.Ops {
		if err := checkRel(op.Path); err != nil {
			return ApplyResult{}, err
		}
		switch op.Op {
		case "delete", "rmdir", "mkdir", "write":
		default:
			return ApplyResult{}, proto.Errorf(proto.CodeInvalidParams, "unknown op %q", op.Op)
		}
		if op.Op != "mkdir" && op.Path == "" {
			return ApplyResult{}, proto.Errorf(proto.CodeInvalidParams, "%s needs a path below the root", op.Op)
		}
	}
	var own *owner
	if p.Owner != "" {
		var err error
		if own, err = lookupOwner(p.Owner); err != nil {
			return ApplyResult{}, err
		}
	}
	s := m.session(p.ID)
	if err := s.lock(p.ID); err != nil {
		return ApplyResult{}, err
	}
	defer s.unlock()
	s.progress.begin("apply")
	defer s.progress.end()
	if !s.loaded || !samePath(s.root, p.Root) {
		m.load(s, p.ID, p.Root)
	}
	// Only a call that runs to the end without committing keeps its writes
	// in progress: a cancel or an error drops them, and the host starts
	// those files again from offset 0.
	keep := false
	defer func() { s.end(keep) }()
	if err := s.openRoot(); err != nil {
		return ApplyResult{}, proto.FromOS(err)
	}
	if own != nil {
		if err := s.dir.Lchown(".", own.uid, own.gid); err != nil {
			return ApplyResult{}, proto.FromOS(err)
		}
	}
	a := applier{s: s, owner: own}
	var batch []Op // consecutive whole-file writes, run in parallel
	flush := func() {
		var wg sync.WaitGroup
		sem := make(chan struct{}, parallelWrites)
		for _, op := range batch {
			sem <- struct{}{}
			wg.Go(func() { defer func() { <-sem }(); a.write(op) })
		}
		wg.Wait()
		batch = batch[:0]
	}
	for _, op := range p.Ops {
		if s.cancelled() {
			flush()
			return ApplyResult{}, proto.Errorf(proto.CodeOS, "the sync of share %s was cancelled", p.ID)
		}
		if op.Op == "write" && op.Offset == 0 && op.End {
			batch = append(batch, op)
			continue
		}
		flush()
		switch op.Op {
		case "delete":
			a.delete(op.Path, op.Expect)
		case "rmdir":
			a.rmdir(op.Path)
		case "mkdir":
			a.mkdir(op.Path)
		case "write":
			a.write(op)
		}
	}
	flush()
	res := ApplyResult{Failures: a.failures, Owner: p.Owner}
	if res.Failures == nil {
		res.Failures = []Failure{}
	}
	keep = !p.Commit
	if p.Commit {
		s.walk = nil
		if err := m.save(s, p.ID); err != nil {
			return res, proto.FromOS(fmt.Errorf("save the guest manifest: %w", err))
		}
		slog.Info("sync committed", "id", p.ID, "failures", len(res.Failures))
	}
	return res, nil
}

// applier runs one call's operations; whole-file writes run concurrently, so
// the shared maps are guarded by mu.
type applier struct {
	s        *session
	owner    *owner
	mu       sync.Mutex
	failures []Failure
}

// fail records err for path; replacing says the operation replaced or
// deleted an existing file, where access denied may also mean in use.
func (a *applier) fail(path string, err error, replacing bool) {
	f := Failure{Path: path, Message: osText(err), Locked: isLocked(err)}
	if !f.Locked && isDenied(err) {
		f.Denied, f.Locked = true, replacing
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = append(a.failures, f)
}

func (a *applier) setRecord(path string, r *record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r == nil {
		delete(a.s.records, path)
	} else {
		a.s.records[path] = *r
	}
}

func native(rel string) string {
	if rel == "" {
		return "."
	}
	return filepath.FromSlash(rel)
}

func (a *applier) delete(rel string, expect *string) {
	if !a.expected(rel, expect) {
		return
	}
	if err := a.s.dir.Remove(native(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.fail(rel, err, true)
		return
	}
	a.setRecord(rel, nil)
}

func (a *applier) rmdir(rel string) {
	err := a.s.dir.Remove(native(rel))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	// A directory that still holds something (excluded files) stays.
	if d, oerr := a.s.dir.Open(native(rel)); oerr == nil {
		entries, _ := d.ReadDir(1)
		d.Close()
		if len(entries) > 0 {
			return
		}
	}
	a.fail(rel, err, true)
}

func (a *applier) mkdir(rel string) {
	if err := a.mkdirAll(native(rel)); err != nil {
		a.fail(rel, err, false)
	}
}

// mkdirAll creates dir and its missing parents below the root, owned by the
// owner when there is one. Lchown never follows a link, and the os.Root
// refuses any component that leaves the root.
func (a *applier) mkdirAll(dir string) error {
	if a.owner == nil {
		return a.s.dir.MkdirAll(dir, 0o755)
	}
	at := ""
	for _, name := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		if name == "." {
			continue
		}
		at = filepath.Join(at, name)
		err := a.s.dir.Mkdir(at, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err == nil {
			err = a.s.dir.Lchown(at, a.owner.uid, a.owner.gid)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// write streams one chunk of a file into a temporary file next to it; the
// chunk marked end checks the hash and renames it over the target.
func (a *applier) write(op Op) {
	s := a.s
	a.mu.Lock()
	failed := s.failed[op.Path]
	w := s.pending[op.Path]
	a.mu.Unlock()
	if op.Offset == 0 {
		if w != nil { // a restart of the same file: drop the old attempt
			w.f.Close()
			s.dir.Remove(w.tmp)
		}
		a.mu.Lock()
		delete(s.failed, op.Path)
		delete(s.pending, op.Path)
		a.mu.Unlock()
		var err error
		if w, err = a.create(op.Path); err != nil {
			a.writeFailed(op.Path, nil, err)
			return
		}
		a.mu.Lock()
		s.pending[op.Path] = w
		a.mu.Unlock()
	} else if failed {
		return // reported with its first chunk
	} else if w == nil || w.offset != op.Offset {
		a.writeFailed(op.Path, w, fmt.Errorf("chunk at offset %d arrived out of order", op.Offset))
		return
	}
	s.progress.at(op.Path)
	s.progress.bytes.Add(int64(len(op.Data)))
	if op.End {
		s.progress.files.Add(1)
	}
	if _, err := w.f.Write(op.Data); err != nil {
		a.writeFailed(op.Path, w, err)
		return
	}
	w.sum.Write(op.Data)
	w.offset += int64(len(op.Data))
	if !op.End {
		return
	}
	a.mu.Lock()
	delete(s.pending, op.Path)
	a.mu.Unlock()
	if got := hex.EncodeToString(w.sum.Sum(nil)); got != op.Hash {
		a.writeFailed(op.Path, w, errors.New("the host file changed while it was sent; sync again"))
		return
	}
	if err := w.f.Close(); err != nil {
		a.writeFailed(op.Path, w, err)
		return
	}
	if !a.expected(op.Path, op.Expect) {
		a.writeFailed(op.Path, w, nil)
		return
	}
	keepMode(s.dir, native(op.Path), w.tmp)
	if err := s.dir.Rename(w.tmp, native(op.Path)); err != nil {
		a.writeFailed(op.Path, w, replaceError{err})
		return
	}
	// Only reads attributes: a link swapped in now could at worst make the
	// recorded key wrong, which costs a rehash on the next sync.
	k, err := fileKey(join(s.root, op.Path))
	if err != nil { // written, but unknown: the next sync hashes it again
		a.setRecord(op.Path, nil)
		return
	}
	a.setRecord(op.Path, &record{k, op.Hash})
}

func (a *applier) create(rel string) (*pendingWrite, error) {
	dir, base := filepath.Split(native(rel))
	if dir != "" {
		if err := a.mkdirAll(dir); err != nil {
			return nil, err
		}
	}
	for {
		tmp := filepath.Join(dir, "."+base+"."+rand.Text()[:12]+".glov-sync")
		f, err := a.s.dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Before the rename, so the file never appears under root's name,
		// and before the stat key is recorded (chown changes ctime).
		if a.owner != nil {
			if err := f.Chown(a.owner.uid, a.owner.gid); err != nil {
				f.Close()
				a.s.dir.Remove(tmp)
				return nil, err
			}
		}
		return &pendingWrite{f: f, tmp: tmp, sum: sha256.New()}, nil
	}
}

func (a *applier) writeFailed(rel string, w *pendingWrite, err error) {
	if w != nil {
		w.f.Close()
		a.s.dir.Remove(w.tmp)
	}
	a.mu.Lock()
	delete(a.s.pending, rel)
	a.s.failed[rel] = true
	a.mu.Unlock()
	if err == nil { // a conflict, reported by expected
		return
	}
	_, replacing := errors.AsType[replaceError](err)
	a.fail(rel, err, replacing)
}

// expected reports whether rel holds what expect says (always, when nil),
// reporting a conflict otherwise. It reads through the root, never following
// a link; the check and the change that follows are not atomic, so a guest
// edit in between is still overwritten.
func (a *applier) expected(rel string, expect *string) bool {
	if expect == nil {
		return true
	}
	got, err := a.current(rel)
	if err != nil {
		a.fail(rel, err, false)
		return false
	}
	if got == *expect {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = append(a.failures, Failure{Path: rel, Message: "the guest file changed since the host last saw it", Conflict: true})
	return false
}

// current is the SHA-256 of rel, "" when there is no file, or "dir" or
// "other" for something else: the recorded hash when its stat key is
// unchanged.
func (a *applier) current(rel string) (string, error) {
	fi, err := a.s.dir.Lstat(native(rel))
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
	k, kerr := fileKey(join(a.s.root, rel))
	a.mu.Lock()
	r, known := a.s.records[rel]
	a.mu.Unlock()
	if known && kerr == nil && r.Key == k {
		return r.Hash, nil
	}
	return a.s.hashFile(a.s.dir, rel)
}

// replaceError marks a failure to rename a written file over its target.
type replaceError struct{ error }

func (e replaceError) Unwrap() error { return e.error }
