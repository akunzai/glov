package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// cs is tinyQCOW2's cluster size: one L2 table then spans 512 MiB, and a
// cluster holds 16 of convert's blocks.
const cs = 64 << 10

// tinyQCOW2 is a version 3 image of size bytes (at most 512 MiB) with one L2
// table and plain clusters at the given cluster indexes.
func tinyQCOW2(size uint64, plain map[uint64][]byte) []byte {
	img := make([]byte, 3*cs) // header, L1, L2
	be := binary.BigEndian
	for idx, data := range plain {
		off := uint64(len(img))
		img = append(img, append(data, make([]byte, cs-len(data))...)...)
		be.PutUint64(img[2*cs+idx*8:], off|1<<63)
	}
	be.PutUint64(img[cs:], 2*cs|1<<63)
	copy(img, "QFI\xfb")
	be.PutUint32(img[4:], 3)
	be.PutUint32(img[20:], 16)
	be.PutUint64(img[24:], size)
	be.PutUint32(img[36:], 1)
	be.PutUint64(img[40:], cs)
	be.PutUint32(img[96:], 4)
	be.PutUint32(img[100:], 104)
	return img
}

func TestConvertWritesASparseRawDiskOfTheVirtualSize(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.qcow2"), filepath.Join(dir, "a.raw")
	const size = 64 << 20 // though the image holds two clusters
	if err := os.WriteFile(src, tinyQCOW2(size, map[uint64][]byte{0: []byte("boot"), 60: []byte("end")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"convert", src, dst}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, size)
	copy(want, "boot")
	copy(want[60*cs:], "end")
	if !bytes.Equal(got, want) {
		t.Fatalf("raw disk of %d bytes differs from the image's data", len(got))
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dst, &st); err != nil {
		t.Fatal(err)
	}
	if st.Blocks*512 > size/8 { // APFS allocates a few MiB for even a small sparse file
		t.Errorf("raw disk uses %d bytes on disk; it should be sparse", st.Blocks*512)
	}
	if info, _ := os.Stat(dst); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
}

func TestConvertKeepsTheTailOfAClusterCutAtTheVirtualSize(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.qcow2"), filepath.Join(dir, "a.raw")
	const size = cs + 512 // the second cluster is cut to 512 bytes, less than a block
	if err := os.WriteFile(src, tinyQCOW2(size, map[uint64][]byte{1: []byte("tail")}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"convert", src, dst}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, size)
	copy(want[cs:], "tail")
	if !bytes.Equal(got, want) {
		t.Fatalf("raw disk of %d bytes lost the last cluster's data", len(got))
	}
}

func TestConvertRefusesAnExistingTargetAndLeavesNoneOnFailure(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.qcow2"), filepath.Join(dir, "a.raw")
	os.WriteFile(src, tinyQCOW2(4096, nil), 0o600)
	os.WriteFile(dst, []byte("keep"), 0o600)
	if err := run([]string{"convert", src, dst}); err == nil {
		t.Fatal("overwrote an existing file")
	}
	if b, _ := os.ReadFile(dst); string(b) != "keep" {
		t.Fatalf("existing file changed to %q", b)
	}
	broken := tinyQCOW2(2*cs, map[uint64][]byte{1: []byte("x")})
	broken = broken[:len(broken)-cs/2] // the data cluster is cut short
	os.WriteFile(src, broken, 0o600)
	other := filepath.Join(dir, "b.raw")
	if err := run([]string{"convert", src, other}); err == nil {
		t.Fatal("converted a truncated image")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatalf("left %s behind: %v", other, err)
	}
	os.WriteFile(src, []byte("not a qcow2 at all"), 0o600)
	if err := run([]string{"convert", src, other}); err == nil {
		t.Fatal("converted a file that is not qcow2")
	}
}

func TestVerifyUbuntu(t *testing.T) {
	sums, sig := "../../pgpsig/testdata/SHA256SUMS", "../../pgpsig/testdata/SHA256SUMS.gpg"
	if err := run([]string{"verify-ubuntu", sums, sig}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify-ubuntu", sig, sums}); err == nil {
		t.Fatal("swapped files verified")
	}
	if err := run([]string{"verify-ubuntu", sums}); err == nil {
		t.Fatal("missing argument accepted")
	}
}
