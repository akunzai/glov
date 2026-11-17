package files_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akunzai/glov/guest/internal/files"
	"github.com/akunzai/glov/guest/proto"
)

func code(t *testing.T, err error) int {
	t.Helper()
	pe, ok := errors.AsType[*proto.Error](err)
	if !ok {
		t.Fatalf("err = %v, want *proto.Error", err)
	}
	return pe.Code
}

func TestWriteThenReadInChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "f.bin")
	data := bytes.Repeat([]byte("0123456789"), 100)

	if _, err := files.Write(files.WriteParams{Path: path, Data: data[:600]}); code(t, err) != proto.CodeNotFound {
		t.Fatalf("write without mkdirs: %v", err)
	}
	res, err := files.Write(files.WriteParams{Path: path, Data: data[:600], Truncate: true, Mkdirs: true})
	if err != nil || res.Size != 600 {
		t.Fatalf("first chunk: %+v, %v", res, err)
	}
	if res, err = files.Write(files.WriteParams{Path: path, Offset: 600, Data: data[600:]}); err != nil || res.Size != 1000 {
		t.Fatalf("second chunk: %+v, %v", res, err)
	}

	var got []byte
	for off := int64(0); ; {
		r, err := files.Read(files.ReadParams{Path: path, Offset: off, Length: 300})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Data...)
		off += int64(len(r.Data))
		if r.EOF {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch")
	}
}

func TestReadEOFSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	os.WriteFile(path, []byte("abcde"), 0o644)
	for _, tc := range []struct {
		off    int64
		length int
		data   string
		eof    bool
	}{
		{0, 5, "abcde", true}, // exact fit is already EOF
		{0, 4, "abcd", false},
		{3, 10, "de", true},
		{9, 10, "", true},
	} {
		r, err := files.Read(files.ReadParams{Path: path, Offset: tc.off, Length: tc.length})
		if err != nil || string(r.Data) != tc.data || r.EOF != tc.eof {
			t.Errorf("Read(off=%d,len=%d) = %q eof=%v err=%v, want %q eof=%v", tc.off, tc.length, r.Data, r.EOF, err, tc.data, tc.eof)
		}
	}
	if _, err := files.Read(files.ReadParams{Path: path, Length: files.MaxRead + 1}); code(t, err) != proto.CodeInvalidParams {
		t.Fatalf("over-long read: %v", err)
	}
}

func TestWriteValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if _, err := files.Write(files.WriteParams{Path: path, Offset: 1, Truncate: true}); code(t, err) != proto.CodeInvalidParams {
		t.Fatalf("truncate at offset 1: %v", err)
	}
	os.WriteFile(path, []byte("long content"), 0o644)
	if r, err := files.Write(files.WriteParams{Path: path, Data: []byte("ab"), Truncate: true}); err != nil || r.Size != 2 {
		t.Fatalf("truncate: %+v, %v", r, err)
	}
}

func TestStatAndList(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("xyz"), 0o644)
	os.Mkdir(filepath.Join(dir, "d"), 0o755)

	s, err := files.Stat(files.PathParams{Path: filepath.Join(dir, "f")})
	if err != nil || !s.Exists || s.IsDir || s.Size != 3 || s.MTime == nil || s.MTime.Location().String() != "UTC" {
		t.Fatalf("stat file: %+v, %v", s, err)
	}
	if s, _ = files.Stat(files.PathParams{Path: dir}); !s.IsDir {
		t.Fatalf("stat dir: %+v", s)
	}
	if s, err = files.Stat(files.PathParams{Path: filepath.Join(dir, "nope")}); err != nil || s.Exists || s.MTime != nil {
		t.Fatalf("stat missing: %+v, %v", s, err)
	}

	l, err := files.List(files.PathParams{Path: dir})
	if err != nil || len(l.Entries) != 2 || l.Entries[0].Name != "d" || !l.Entries[0].IsDir || l.Entries[1].Size != 3 {
		t.Fatalf("list: %+v, %v", l, err)
	}
	if _, err := files.List(files.PathParams{Path: filepath.Join(dir, "nope")}); code(t, err) != proto.CodeNotFound {
		t.Fatalf("list missing: %v", err)
	}
	if _, err := files.Stat(files.PathParams{}); code(t, err) != proto.CodeInvalidParams {
		t.Fatalf("empty path: %v", err)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(filepath.Join(sub, "x"), 0o755)
	os.WriteFile(filepath.Join(sub, "x", "f"), nil, 0o644)

	if _, err := files.Remove(files.RemoveParams{Path: sub}); err == nil {
		t.Fatal("removed a non-empty directory without recursive")
	}
	if _, err := files.Remove(files.RemoveParams{Path: sub, Recursive: true}); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []bool{false, true} {
		if _, err := files.Remove(files.RemoveParams{Path: sub, Recursive: rec}); code(t, err) != proto.CodeNotFound {
			t.Fatalf("remove missing (recursive=%v): %v", rec, err)
		}
	}
}
