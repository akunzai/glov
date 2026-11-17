package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/share"
)

// syncDaemon stands in for glovd and the Guest Service: it holds the
// guest copy of one share in memory and answers as they would.
type syncDaemon struct {
	t        *testing.T
	share    share.Share
	files    map[string][]byte
	dirs     map[string]bool
	pending  map[string][]byte
	held     bool
	cancels  int
	applies  int
	maxFrame int
	stages   []string
	// beforeApply runs before each sync.apply.
	beforeApply func()
	// fail names files whose writes fail as access denied.
	fail map[string]bool
}

func newSyncDaemon(t *testing.T, host string, exclude ...string) *syncDaemon {
	return &syncDaemon{t: t, share: share.Share{Name: "proj", HostPath: host, Mode: share.RW, Exclude: exclude},
		files: map[string][]byte{}, dirs: map[string]bool{}, pending: map[string][]byte{}}
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (d *syncDaemon) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := json.Marshal(params)
	d.maxFrame = max(d.maxFrame, len(b))
	var res any = struct{}{}
	switch method {
	case api.MethodShareSyncBegin:
		if d.held {
			return errors.New("already running")
		}
		d.held = true
		res = api.ShareSyncBegun{VM: "dev", Share: api.NewShareInfo(d.share)}
	case api.MethodShareSyncProgress:
		var p api.ShareSyncProgressParams
		json.Unmarshal(b, &p)
		if len(d.stages) == 0 || d.stages[len(d.stages)-1] != p.Stage {
			d.stages = append(d.stages, p.Stage)
		}
	case api.MethodShareSyncEnd:
		var p api.ShareSyncEndParams
		json.Unmarshal(b, &p)
		d.held = false
		if p.Cancel {
			d.cancels++
		}
	case api.MethodShareSyncCall:
		var p api.ShareSyncCallParams
		json.Unmarshal(b, &p)
		var err error
		if res, err = d.guest(p.Method, p.Params); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err // Ctrl+C while the call ran
		}
	default:
		return fmt.Errorf("unexpected %s", method)
	}
	if result == nil {
		return nil
	}
	out, _ := json.Marshal(res)
	return json.Unmarshal(out, result)
}

func (d *syncDaemon) guest(method string, raw []byte) (any, error) {
	switch method {
	case "sync.manifest":
		var p struct {
			Subdir  string       `json:"subdir"`
			Exclude []share.Rule `json:"exclude"`
		}
		json.Unmarshal(raw, &p)
		// The real Guest Service applies p.Exclude; these are its patterns.
		ex, _ := share.CompileExcludes(d.share.Exclude)
		if want, _ := share.CompileExcludes(d.share.Exclude); !slices.Equal(p.Exclude, want.Rules) && len(want.Rules) > 0 {
			d.t.Errorf("exclude rules = %v, want %v", p.Exclude, want.Rules)
		}
		var entries []map[string]any
		in := func(x string) bool { return p.Subdir == "" || strings.HasPrefix(x, p.Subdir+"/") }
		for _, dir := range slices.Sorted(maps.Keys(d.dirs)) {
			if in(dir) && !ex.ExcludedOrParent(dir, true) {
				entries = append(entries, map[string]any{"path": dir, "dir": true})
			}
		}
		for _, f := range slices.Sorted(maps.Keys(d.files)) {
			if in(f) && !ex.ExcludedOrParent(f, false) {
				entries = append(entries, map[string]any{"path": f, "size": len(d.files[f]), "hash": hexSHA(d.files[f])})
			}
		}
		return map[string]any{"entries": entries, "total": len(entries), "hashed": len(entries)}, nil
	case "sync.apply":
		if d.beforeApply != nil {
			d.beforeApply()
		}
		d.applies++
		var p struct {
			Ops []struct {
				Op     string `json:"op"`
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Data   []byte `json:"data"`
				End    bool   `json:"end"`
				Hash   string `json:"hash"`
			} `json:"ops"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		var failures []syncFailure
		for _, op := range p.Ops {
			switch op.Op {
			case "delete":
				delete(d.files, op.Path)
			case "rmdir":
				empty := true
				for f := range d.files {
					empty = empty && !strings.HasPrefix(f, op.Path+"/")
				}
				if empty {
					delete(d.dirs, op.Path)
				}
			case "mkdir":
				for x := op.Path; x != "" && x != "."; x = path.Dir(x) {
					d.dirs[x] = true
				}
			case "write":
				if op.Offset != len(d.pending[op.Path]) {
					d.t.Errorf("%s: offset %d after %d bytes", op.Path, op.Offset, len(d.pending[op.Path]))
				}
				d.pending[op.Path] = append(d.pending[op.Path], op.Data...)
				if op.End {
					data := d.pending[op.Path]
					delete(d.pending, op.Path)
					if d.fail[op.Path] {
						failures = append(failures, syncFailure{Path: op.Path, Message: "permission denied", Denied: true})
						continue
					}
					if hexSHA(data) != op.Hash {
						failures = append(failures, syncFailure{Path: op.Path, Message: "the host file changed while it was sent"})
						continue
					}
					d.files[op.Path] = data
				}
			}
		}
		return map[string]any{"failures": failures}, nil
	}
	return nil, fmt.Errorf("unexpected guest method %s", method)
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// canonicalTemp is a new folder named as share.Canonical names it.
func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	os.Mkdir(dir, 0o755)
	c, err := share.Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (d *syncDaemon) sync(ctx context.Context, ref string, verify bool) (syncResult, error) {
	cache := d.t.TempDir()
	s := &shareSync{c: d, ctx: ctx, vm: "de", cacheDir: func(vm string) (string, error) {
		if vm != "dev" {
			d.t.Errorf("cache of VM %q", vm)
		}
		return cache, nil
	}}
	return s.run(ref, verify)
}

func TestShareSyncMirrorsTheHostFolderAndKeepsExcludedGuestFiles(t *testing.T) {
	host := canonicalTemp(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), applyChunk/8+3) // over two chunks
	writeTree(t, host, map[string]string{"a.txt": "a", "src/b.cs": "bb", "src/old.cs": "old", "bin/x.dll": "host build", "big.bin": string(big)})
	os.Mkdir(filepath.Join(host, "empty"), 0o755)
	d := newSyncDaemon(t, host, "bin/")
	d.files["bin/y.dll"] = []byte("guest build")
	d.files["stale.txt"] = []byte("stale")
	d.dirs["bin"], d.dirs["gone"] = true, true

	r, err := d.sync(t.Context(), "proj", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.written != 4 || r.deleted != 1 || r.guestPath != (share.Share{HostPath: host}).GuestPath() || r.hostHashed != 4 {
		t.Errorf("first sync = %+v", r)
	}
	want := map[string]string{"a.txt": "a", "src/b.cs": "bb", "src/old.cs": "old", "big.bin": string(big), "bin/y.dll": "guest build"}
	check := func() {
		t.Helper()
		got := map[string]string{}
		for k, v := range d.files {
			got[k] = string(v)
		}
		if !maps.Equal(got, want) {
			t.Errorf("guest files = %v", slices.Sorted(maps.Keys(got)))
		}
	}
	check()
	if !d.dirs["empty"] || d.dirs["gone"] || !d.dirs["bin"] {
		t.Errorf("guest dirs = %v", d.dirs)
	}
	if d.maxFrame > 1<<20-4096 || d.applies < 2 {
		t.Errorf("largest frame %d bytes, %d apply calls", d.maxFrame, d.applies)
	}
	if !slices.Equal(d.stages, []string{api.StageHostScan, api.StageGuestListing, api.StageApplying}) {
		t.Errorf("stages = %q", d.stages)
	}

	// Edit, delete and rename on the host.
	writeTree(t, host, map[string]string{"a.txt": "A!"})
	os.Remove(filepath.Join(host, "big.bin"))
	os.Rename(filepath.Join(host, "src", "old.cs"), filepath.Join(host, "src", "new.cs"))
	r, err = d.sync(t.Context(), "proj", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.written != 2 || r.deleted != 2 || r.unchanged != 1 || !slices.Equal(r.writtenPaths, []string{"a.txt", "src/new.cs"}) {
		t.Errorf("second sync = %+v", r)
	}
	want = map[string]string{"a.txt": "A!", "src/b.cs": "bb", "src/new.cs": "old", "bin/y.dll": "guest build"}
	check()
	if out := r.describe(); !strings.Contains(out, "+ src/new.cs") || !strings.Contains(out, "- big.bin") {
		t.Errorf("describe = %s", out)
	}

	// One subdirectory.
	writeTree(t, host, map[string]string{"src/b.cs": "BB", "a.txt": "not yet"})
	if r, err = d.sync(t.Context(), `proj\src`, false); err != nil || r.written != 1 || !strings.HasSuffix(r.guestPath, "/src") {
		t.Fatalf("subdir sync = %+v, %v", r, err)
	}
	if string(d.files["src/b.cs"]) != "BB" || string(d.files["a.txt"]) != "A!" {
		t.Error("the subdirectory sync went outside src")
	}
	if _, err := d.sync(t.Context(), "proj/bin", false); err == nil || !strings.Contains(err.Error(), "excluded") {
		t.Errorf("sync of an excluded directory = %v", err)
	}
	if _, err := d.sync(t.Context(), "proj/nope", false); err == nil {
		t.Error("sync of a missing directory")
	}
	if d.held {
		t.Error("the share stayed held")
	}
}

func TestShareSyncReportsFilesTheGuestRefused(t *testing.T) {
	host := canonicalTemp(t)
	writeTree(t, host, map[string]string{"ok.txt": "ok", "ro.txt": "ro"})
	d := newSyncDaemon(t, host)
	d.fail = map[string]bool{"ro.txt": true}
	_, err := d.sync(t.Context(), "proj", false)
	if err == nil || !strings.Contains(err.Error(), "access denied in the guest") || !strings.Contains(err.Error(), "ro.txt (permission denied)") ||
		!strings.Contains(err.Error(), "1 written") {
		t.Errorf("err = %v", err)
	}
	if string(d.files["ok.txt"]) != "ok" || d.cancels != 1 {
		t.Errorf("files %v, cancels %d", d.files, d.cancels)
	}
}

func TestCancellingAShareSyncCancelsItInTheGuest(t *testing.T) {
	host := canonicalTemp(t)
	writeTree(t, host, map[string]string{"a": strings.Repeat("a", applyChunk), "b": "b"})
	d := newSyncDaemon(t, host)
	ctx, cancel := context.WithCancel(t.Context())
	d.beforeApply = cancel
	_, err := d.sync(ctx, "proj", false)
	if err == nil || !strings.Contains(err.Error(), "was cancelled") || d.cancels != 1 || d.held {
		t.Errorf("err = %v, cancels %d, held %v", err, d.cancels, d.held)
	}
}
