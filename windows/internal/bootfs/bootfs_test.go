package bootfs

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// The fixtures are fixed VHDs made by Windows diskpart: a GPT disk with one
// EFI system partition, formatted FAT32 (fat32.vhd) or FAT16 (fat16.vhd),
// holding the files below.
var fixtures = []string{"fat32", "fat16"}

func load(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open("testdata/" + name + ".vhd.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func kernelText() string {
	lines := make([]string, 20000)
	for i := range lines {
		lines[i] = fmt.Sprintf("kernel line %d", i+1)
	}
	return strings.Join(lines, "\n")
}

func openFS(t *testing.T, img []byte) *FS {
	t.Helper()
	disk, err := FixedVHD(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	esp, err := ESP(disk)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := OpenFAT(esp)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestReadsFilesByLongAndShortNames(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			img := load(t, name)
			fs := openFS(t, img)
			if fs.fat32 != (name == "fat32") {
				t.Fatalf("fat32 = %v", fs.fat32)
			}
			for path, want := range map[string]string{
				"vmlinuz-virt":   kernelText(), // spans many clusters
				"initramfs-virt": "initramfs",
				"/limine/Some Long Directory/Mixed Case Name.conf": "nested",
				"LIMINE/some long directory/mixed case name.CONF":  "nested",
				"short.txt": "short",
				"empty":     "",
			} {
				got, err := fs.ReadFile(path)
				if err != nil || string(got) != want {
					t.Errorf("ReadFile(%q) = %d bytes, %v; want %d bytes", path, len(got), err, len(want))
				}
			}
			for _, path := range []string{"missing", "limine", "vmlinuz-virt/x", "limine/missing"} {
				if _, err := fs.ReadFile(path); err == nil {
					t.Errorf("ReadFile(%q) succeeded", path)
				}
			}
		})
	}
}

func TestRefusesWhatIsNotAFixedVHD(t *testing.T) {
	img := load(t, "fat32")
	size := int64(len(img))
	dynamic := bytes.Clone(img)
	binary.BigEndian.PutUint32(dynamic[size-512+60:], 3)
	truncated := img[:size-4096]
	for name, b := range map[string][]byte{"dynamic": dynamic, "truncated": truncated, "raw": img[:size-512], "tiny": img[:100]} {
		if _, err := FixedVHD(bytes.NewReader(b), int64(len(b))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fatEntryOffset returns where cluster c's FAT entry lives in img.
func fatEntryOffset(t *testing.T, img []byte, fs *FS, c uint32) int64 {
	t.Helper()
	disk, _ := FixedVHD(bytes.NewReader(img), int64(len(img)))
	esp, _ := ESP(disk)
	_, base, _ := esp.Outer()
	width := int64(2)
	if fs.fat32 {
		width = 4
	}
	return base + fs.fatOffset + int64(c)*width
}

func firstCluster(t *testing.T, fs *FS, name string) uint32 {
	t.Helper()
	root, err := fs.root()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries(root) {
		if e.name == name {
			return e.cluster
		}
	}
	t.Fatalf("%s not in root", name)
	return 0
}

func TestRefusesBrokenClusterChains(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				what string
				next uint32
				msg  string
			}{
				{"loop", 0, "loops back"}, // points back at itself
				{"out of range", 0xFFF0, "points to"},
				{"cut short", 0xFFFFFFFF, "shorter than the file"},
			} {
				img := load(t, name)
				fs := openFS(t, img)
				c := firstCluster(t, fs, "vmlinuz-virt")
				next := tc.next
				if tc.what == "loop" {
					next = c
				}
				off := fatEntryOffset(t, img, fs, c)
				if fs.fat32 {
					binary.LittleEndian.PutUint32(img[off:], next)
				} else {
					binary.LittleEndian.PutUint16(img[off:], uint16(next))
				}
				_, err := openFS(t, img).ReadFile("vmlinuz-virt")
				if err == nil || !strings.Contains(err.Error(), tc.msg) {
					t.Errorf("%s: err = %v, want it to mention %q", tc.what, err, tc.msg)
				}
			}
		})
	}
}
