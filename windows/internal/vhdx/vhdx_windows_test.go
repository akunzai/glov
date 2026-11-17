package vhdx

import (
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Microsoft/go-winio/vhd"
)

func fixtureVHD(t *testing.T) string {
	t.Helper()
	f, err := os.Open("../bootfs/testdata/fat32.vhd.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "src.vhd")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, z); err != nil {
		t.Fatal(err)
	}
	return path
}

// opens checks that virtdisk accepts the file as a virtual disk (for a
// differencing disk, that includes finding its parent).
func opens(t *testing.T, path string) {
	t.Helper()
	h, err := vhd.OpenVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.OpenVirtualDiskFlagNone)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	syscall.CloseHandle(h)
}

// virtualSize reads a disk's virtual size as diskpart reports it.
func virtualSize(t *testing.T, path string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "dp.txt")
	os.WriteFile(script, []byte("select vdisk file=\""+path+"\"\r\ndetail vdisk\r\n"), 0o600)
	out, err := exec.Command("diskpart", "/s", script).CombinedOutput()
	if err != nil {
		t.Skipf("diskpart: %v", err)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, "Virtual size:"); ok && strings.TrimSpace(k) == "" {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no virtual size for %s:\n%s", path, out)
	return ""
}

func TestConvertAndDifferencing(t *testing.T) {
	src := fixtureVHD(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.vhdx")
	if err := FromVHD(src, base); err != nil {
		t.Fatal(err)
	}
	opens(t, base)
	if _, err := os.Stat(base + ".part.vhdx"); !os.IsNotExist(err) {
		t.Fatal("the .part file was left behind")
	}
	child := filepath.Join(dir, "vm", "disk.vhdx")
	os.MkdirAll(filepath.Dir(child), 0o700)
	if err := Differencing(child, base, 1<<30); err != nil {
		t.Fatal(err)
	}
	opens(t, child)
	if base, child := virtualSize(t, base), virtualSize(t, child); base != "64 MB" || child != "1024 MB" {
		t.Fatalf("virtual sizes: base %s, child %s; want 64 MB and 1024 MB", base, child)
	}
	if err := Differencing(child, base, 1<<30); err == nil {
		t.Fatal("Differencing overwrote an existing disk")
	}
}

func TestDifferencingRefusesToShrink(t *testing.T) {
	src := fixtureVHD(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.vhdx")
	if err := FromVHD(src, base); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "disk.vhdx")
	if err := Differencing(child, base, 16<<20); err == nil {
		t.Fatal("Differencing shrank the disk")
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Fatal("a failed Differencing left the disk behind")
	}
}

// A template chain: base, then template (the frozen disk), then child; a
// merge folds the child into the template, which stays a valid disk on
// base, and a new child opens on it.
func TestMergeFoldsAChildIntoItsParent(t *testing.T) {
	src := fixtureVHD(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.vhdx")
	if err := FromVHD(src, base); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(dir, "template.vhdx")
	child := filepath.Join(dir, "disk.vhdx")
	if err := Differencing(template, base, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := Differencing(child, template, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := Merge(child); err != nil {
		t.Fatal(err)
	}
	opens(t, template)
	os.Remove(child)
	if err := Differencing(child, template, 2<<30); err != nil {
		t.Fatal(err)
	}
	opens(t, child)
	if err := Merge(filepath.Join(dir, "missing.vhdx")); err == nil {
		t.Error("merged a missing disk")
	}
}
