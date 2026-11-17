package live

import (
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/akunzai/glov/windows/internal/share"
)

// Call calls a Guest Service sync method for the share through glovd
// (share.syncCall), which sets its id, root and owner.
type Call func(method string, params, result any) error

// sync.apply and sync.read limits: a call stays well under the 1 MiB frame,
// as data is base64 (4/3) and each op carries its path.
const (
	applyBudget = 900 << 10
	applyMaxOps = 256
	readChunk   = 512 << 10
	statPaths   = 512
)

// Guest is the guest side of a live share, through the Guest Service:
// sync.manifest lists and hashes, sync.read reads, sync.apply changes (in
// batches, sent once full and at Flush).
type Guest struct {
	call  Call
	rules []share.Rule

	ops      []map[string]any
	size     int
	sent     bool // since the last commit
	failures map[string]error
}

// NewGuest serves the share through call, with exclude rules.
func NewGuest(call Call, rules []share.Rule) *Guest {
	if rules == nil {
		rules = []share.Rule{}
	}
	return &Guest{call: call, rules: rules, failures: map[string]error{}}
}

func (g *Guest) Name() string { return "guest" }

type entry struct {
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Mtime int64  `json:"mtime"`
	Hash  string `json:"hash"`
	Error string `json:"error"`
}

func (e entry) state() State {
	switch {
	case e.Dir:
		return State{Dir: true}
	case e.Error != "":
		return State{Other: true}
	}
	return State{Hash: e.Hash, Mtime: e.Mtime}
}

type manifestPage struct {
	Entries []entry `json:"entries"`
	Total   int     `json:"total"`
}

func (g *Guest) List(scope string) (Snapshot, error) {
	out := Snapshot{}
	for received := 0; ; {
		var page manifestPage
		if err := g.call("sync.manifest", map[string]any{"subdir": scope, "exclude": g.rules, "offset": received}, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			out[e.Path] = e.state()
		}
		received += len(page.Entries)
		if received >= page.Total || len(page.Entries) == 0 {
			return out, nil
		}
	}
}

func (g *Guest) Stat(paths []string) (Snapshot, error) {
	out := Snapshot{}
	for chunk := range slices.Chunk(paths, statPaths) {
		var page manifestPage
		if err := g.call("sync.manifest", map[string]any{"paths": chunk, "exclude": g.rules}, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			out[e.Path] = e.state()
		}
	}
	return out, nil
}

// reader reads a guest file chunk by chunk with sync.read; a file whose stat
// key changes between chunks fails with ErrChanged.
type reader struct {
	g      *Guest
	path   string
	offset int64
	key    string
	buf    []byte
	eof    bool
}

func (g *Guest) Open(p string) (io.ReadCloser, error) { return &reader{g: g, path: p}, nil }

func (r *reader) Read(b []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		var res struct {
			Data []byte `json:"data"`
			Key  string `json:"key"`
			EOF  bool   `json:"eof"`
		}
		if err := r.g.call("sync.read", map[string]any{"path": r.path, "offset": r.offset}, &res); err != nil {
			return 0, err
		}
		if r.key != "" && res.Key != r.key {
			return 0, ErrChanged
		}
		r.key, r.buf, r.eof = res.Key, res.Data, res.EOF
		r.offset += int64(len(res.Data))
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *reader) Close() error { return nil }

func (g *Guest) add(op map[string]any, cost int) error {
	if len(g.ops) > 0 && (g.size+cost > applyBudget || len(g.ops) >= applyMaxOps) {
		if err := g.send(false); err != nil {
			return err
		}
	}
	g.ops = append(g.ops, op)
	g.size += cost
	return nil
}

type failure struct {
	Path     string `json:"path"`
	Message  string `json:"message"`
	Conflict bool   `json:"conflict"`
}

func (g *Guest) send(commit bool) error {
	var res struct {
		Failures []failure `json:"failures"`
	}
	ops := g.ops
	if ops == nil {
		ops = []map[string]any{}
	}
	g.ops, g.size = nil, 0
	if err := g.call("sync.apply", map[string]any{"ops": ops, "commit": commit}, &res); err != nil {
		for _, op := range ops {
			g.failures[op["path"].(string)] = err
		}
		return err
	}
	g.sent = !commit
	for _, f := range res.Failures {
		switch {
		case f.Conflict:
			g.failures[f.Path] = ErrConflict
		case strings.Contains(f.Message, "changed while it was sent"):
			g.failures[f.Path] = ErrChanged
		default:
			g.failures[f.Path] = errors.New(f.Message)
		}
	}
	return nil
}

func (g *Guest) Put(p string, r io.Reader, hash, expect string) error {
	buf := make([]byte, readChunk)
	for offset := int64(0); ; {
		want := readChunk
		if room := applyBudget - g.size - 200 - len(p); room >= 4096 {
			want = min(readChunk, room*3/4)
		}
		n, err := io.ReadFull(r, buf[:want])
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		end := n < want
		op := map[string]any{"op": "write", "path": p, "offset": offset, "data": append([]byte(nil), buf[:n]...)}
		if end {
			op["end"], op["hash"], op["expect"] = true, hash, expect
		}
		if err := g.add(op, 200+len(p)+(n+2)/3*4); err != nil {
			return err
		}
		offset += int64(n)
		if end {
			return nil
		}
	}
}

func (g *Guest) Remove(p, expect string) error {
	return g.add(map[string]any{"op": "delete", "path": p, "expect": expect}, 96+len(p))
}

func (g *Guest) Mkdir(p string) error {
	return g.add(map[string]any{"op": "mkdir", "path": p}, 64+len(p))
}

func (g *Guest) Rmdir(p string) error {
	return g.add(map[string]any{"op": "rmdir", "path": p}, 64+len(p))
}

// Flush sends what is pending and commits the guest manifest, then returns
// the paths that failed since the last Flush.
func (g *Guest) Flush() map[string]error {
	if len(g.ops) > 0 || g.sent {
		g.send(true) // a failure marks the paths it carried
	}
	out := g.failures
	g.failures = map[string]error{}
	return out
}
