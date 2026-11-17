package baseimage

import (
	"errors"
	"os"
	"testing"
)

func TestGetFindsOnlyACompleteRelease(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	b := c.base("debian-test")
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeDisk(b, func(dst string) error { return errors.New("conversion failed") }); err == nil {
		t.Fatal("a failed conversion succeeded")
	}
	if _, err := c.Get("debian-test"); err == nil {
		t.Fatal("a failed conversion left a usable release")
	}
	if err := writeDisk(b, func(dst string) error { return os.WriteFile(dst, []byte("vhdx"), 0o600) }); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get("debian-test")
	if err != nil || got != b {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "..", `..\x`, "a/b", "C:x", "missing"} {
		if _, err := c.Get(bad); err == nil {
			t.Errorf("Get(%q) succeeded", bad)
		}
	}
}
