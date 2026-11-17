package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// memGuest is a guest file system in memory, speaking the Guest Service's
// file methods through JSON as glovd would pass them on.
type memGuest struct {
	files map[string][]byte
	dirs  map[string]bool
	calls int
}

func newMemGuest() *memGuest {
	return &memGuest{files: map[string][]byte{}, dirs: map[string]bool{"/": true}}
}

func (g *memGuest) mkdirAll(p string) {
	for ; p != "/" && p != "."; p = path.Dir(p) {
		g.dirs[p] = true
	}
}

func (g *memGuest) mkdirs(dirs []string) error {
	for _, d := range dirs {
		g.mkdirAll(d)
	}
	return nil
}

func (g *memGuest) call(method string, params, result any) error {
	g.calls++
	b, _ := json.Marshal(params)
	var p struct {
		Path     string `json:"path"`
		Offset   int    `json:"offset"`
		Length   int    `json:"length"`
		Data     []byte `json:"data"`
		Truncate bool   `json:"truncate"`
		Mkdirs   bool   `json:"mkdirs"`
	}
	json.Unmarshal(b, &p)
	var res any
	switch method {
	case "file.stat":
		f, isFile := g.files[p.Path]
		res = map[string]any{"exists": isFile || g.dirs[p.Path], "isDir": g.dirs[p.Path], "size": len(f)}
	case "file.list":
		var entries []map[string]any
		for name := range g.files {
			if path.Dir(name) == p.Path {
				entries = append(entries, map[string]any{"name": path.Base(name), "isDir": false})
			}
		}
		for name := range g.dirs {
			if name != "/" && path.Dir(name) == p.Path {
				entries = append(entries, map[string]any{"name": path.Base(name), "isDir": true})
			}
		}
		res = map[string]any{"entries": entries}
	case "file.read":
		f := g.files[p.Path]
		end := min(len(f), p.Offset+p.Length)
		res = map[string]any{"data": f[p.Offset:end], "eof": end == len(f)}
	case "file.write":
		if p.Mkdirs {
			g.mkdirAll(path.Dir(p.Path))
		}
		if p.Truncate {
			g.files[p.Path] = nil
		}
		g.files[p.Path] = append(g.files[p.Path][:p.Offset], p.Data...)
		res = map[string]any{"size": len(g.files[p.Path])}
	}
	if result != nil {
		b, _ := json.Marshal(res)
		return json.Unmarshal(b, result)
	}
	return nil
}

func TestCopyTreeRoundTrip(t *testing.T) {
	src := t.TempDir()
	big := bytes.Repeat([]byte("0123456789"), chunk/5) // two chunks
	for name, data := range map[string][]byte{"a.txt": []byte("hello"), "sub/big.bin": big, "sub/deeper/empty": nil} {
		p := filepath.Join(src, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
	}
	os.Mkdir(filepath.Join(src, "emptydir"), 0o755)

	g := newMemGuest()
	c, err := toGuest(g, src, "/root/proj")
	if err != nil || c.files != 3 || c.bytes != int64(5+len(big)) {
		t.Fatalf("toGuest = %+v, %v", c, err)
	}
	if string(g.files["/root/proj/a.txt"]) != "hello" || !bytes.Equal(g.files["/root/proj/sub/big.bin"], big) || !g.dirs["/root/proj/emptydir"] {
		t.Fatalf("guest files: %v", slices.Sorted(func(yield func(string) bool) {
			for k := range g.files {
				if !yield(k) {
					return
				}
			}
		}))
	}
	if f, ok := g.files["/root/proj/sub/deeper/empty"]; !ok || len(f) != 0 {
		t.Error("empty file not copied")
	}

	dst := filepath.Join(t.TempDir(), "back")
	c, err = fromGuest(g, "/root/proj", dst)
	if err != nil || c.files != 3 {
		t.Fatalf("fromGuest = %+v, %v", c, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "big.bin")); !bytes.Equal(b, big) {
		t.Error("big.bin differs after the round trip")
	}
	if fi, err := os.Stat(filepath.Join(dst, "emptydir")); err != nil || !fi.IsDir() {
		t.Error("empty directory not copied back")
	}
}

func TestCopySingleFileAndMissingSource(t *testing.T) {
	g := newMemGuest()
	g.files["/etc/hostname"] = []byte("dev\n")
	g.mkdirAll("/etc")
	dst := filepath.Join(t.TempDir(), "hostname")
	if c, err := fromGuest(g, "/etc/hostname", dst); err != nil || c.files != 1 || c.bytes != 4 {
		t.Fatalf("fromGuest file = %+v, %v", c, err)
	}
	if _, err := fromGuest(g, "/nope", dst); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing source err = %v", err)
	}
	if _, err := toGuest(g, filepath.Join(t.TempDir(), "nope"), "/x"); err == nil {
		t.Error("copied a missing host file")
	}
}

func TestParseCpPath(t *testing.T) {
	for s, want := range map[string]cpPath{
		"dev:/root/x": {vm: "dev", path: "/root/x"},
		`C:\work`:     {path: `C:\work`},
		"c:/work":     {path: "c:/work"},
		"./proj":      {path: "./proj"},
		"Dev:/x":      {path: "Dev:/x"},
		"dev:":        {vm: "dev"},
	} {
		if got := parseCpPath(s); got != want {
			t.Errorf("parseCpPath(%q) = %+v, want %+v", s, got, want)
		}
	}
}
