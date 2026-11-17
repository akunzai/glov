package seed

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/bootfs"
)

func read(t *testing.T, path string) *bootfs.FS {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := bootfs.FixedVHD(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	fs, err := bootfs.OpenFAT(disk)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestWriteReadsBack(t *testing.T) {
	big := bytes.Repeat([]byte("glov-guest "), 500_000) // spans many clusters
	files := []File{
		{"meta-data", []byte("instance-id: x\n")},
		{"user-data", []byte("#!/bin/sh\necho hi\n")},
		{"glov-guest", big},
		{"empty", nil},
		{"a name longer than thirteen characters.txt", []byte("long")},
	}
	path := filepath.Join(t.TempDir(), "seed.vhd")
	if err := Write(path, files); err != nil {
		t.Fatal(err)
	}
	fs := read(t, path)
	for _, f := range files {
		got, err := fs.ReadFile(f.Name)
		if err != nil || !bytes.Equal(got, f.Data) {
			t.Errorf("%s: %d bytes, %v; want %d bytes", f.Name, len(got), err, len(f.Data))
		}
	}
	b, _ := os.ReadFile(path)
	if label := string(b[43:54]); label != "CIDATA     " {
		t.Errorf("boot sector label %q", label)
	}
	if err := Write(path, files); err == nil {
		t.Error("Write replaced an existing file")
	}
}

func TestFooterChecksumAndGeometry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.vhd")
	if err := Write(path, []File{{"user-data", []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	f := bytes.Clone(b[len(b)-512:])
	want := binary.BigEndian.Uint32(f[64:])
	clear(f[64:68])
	var sum uint32
	for _, c := range f {
		sum += uint32(c)
	}
	if ^sum != want {
		t.Fatalf("checksum %08x, want %08x", want, ^sum)
	}
	// 4,128 sectors of a small seed: the specification's example path.
	if c, h, s := geometry(4128); uint64(c)*uint64(h)*uint64(s) > 4128 || s != 17 {
		t.Fatalf("geometry(4128) = %d/%d/%d", c, h, s)
	}
}

func TestRefusesBadNames(t *testing.T) {
	for _, name := range []string{"", strings.Repeat("x", 256)} {
		if err := Write(filepath.Join(t.TempDir(), "s.vhd"), []File{{name, nil}}); err == nil {
			t.Errorf("name of %d chars accepted", len(name))
		}
	}
}
