//go:build unix

package mirror_test

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/akunzai/glov/guest/internal/mirror"
	"github.com/akunzai/glov/guest/proto"
)

// TestApplyOwner gives what a call creates or writes to its owner. As root
// (CI in a container, the guest itself) the owner is nobody; otherwise
// chowning to anyone else is refused, so it is the current user.
func TestApplyOwner(t *testing.T) {
	name := "nobody"
	if os.Geteuid() != 0 {
		u, err := user.Current()
		if err != nil {
			t.Skip(err)
		}
		name = u.Username
	}
	u, err := user.Lookup(name)
	if err != nil {
		t.Skip(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	f := newFixture(t)
	f.put("old/kept.txt", "x") // already there: keeps its owner
	res, err := f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Owner: name, Commit: true, Ops: []mirror.Op{
		{Op: "mkdir", Path: "a/b"},
		{Op: "write", Path: "c/d/e.txt", Data: []byte("hi"), End: true, Hash: sum("hi")},
		{Op: "write", Path: "old/new.txt", Data: []byte("yo"), End: true, Hash: sum("yo")},
	}})
	if err != nil || len(res.Failures) != 0 || res.Owner != name {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	for _, rel := range []string{"", "a", "a/b", "c", "c/d", "c/d/e.txt", "old/new.txt"} {
		fi, err := os.Lstat(filepath.Join(f.root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := int(fi.Sys().(*syscall.Stat_t).Uid); got != uid {
			t.Errorf("%q is owned by uid %d, want %d (%s)", rel, got, uid, name)
		}
	}
	// The stat key is recorded after the chown: nothing rehashes.
	_, r := f.manifest(mirror.ManifestParams{})
	if r.Hashed != 1 { // old/kept.txt, never recorded
		t.Errorf("a sync after an owned apply hashed %d files, want 1", r.Hashed)
	}

	if m, err := f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root, Owner: name}); err != nil || m.Owner != name {
		t.Errorf("manifest with an owner = %+v, %v; want it echoed", m.Owner, err)
	}
	_, err = f.m.Apply(mirror.ApplyParams{ID: "proj", Root: f.root, Owner: "no-such-user-glov"})
	if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeInvalidParams {
		t.Errorf("an unknown owner err = %v, want invalid params", err)
	}
	_, err = f.m.Manifest(mirror.ManifestParams{ID: "proj", Root: f.root, Owner: "no-such-user-glov"})
	if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeInvalidParams {
		t.Errorf("a manifest with an unknown owner err = %v, want invalid params", err)
	}
}
