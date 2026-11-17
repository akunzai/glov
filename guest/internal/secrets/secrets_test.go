//go:build unix

package secrets_test

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"testing"

	"github.com/akunzai/glov/guest/internal/secrets"
	"github.com/akunzai/glov/guest/proto"
)

func store(t *testing.T) *secrets.Store {
	dir := filepath.Join(t.TempDir(), "secrets")
	return &secrets.Store{Dir: dir, Prepare: func(d string) error { return os.MkdirAll(d, 0o711) }}
}

func me(t *testing.T) string {
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	return u.Username
}

func TestSyncWritesEachSecretReadableByItsOwnerOnly(t *testing.T) {
	s := store(t)
	r, err := s.Sync(secrets.SyncParams{Owner: me(t), Secrets: []secrets.Secret{{Name: "b", Data: []byte("two")}, {Name: "a", Data: []byte("one")}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Names, []string{"a", "b"}) {
		t.Fatalf("names = %v", r.Names)
	}
	fi, err := os.Lstat(filepath.Join(s.Dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != 0o400 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, "b")); string(b) != "two" {
		t.Fatalf("b = %q", b)
	}
}

func TestSyncReplacesValuesAndRemovesWhatIsNoLongerGiven(t *testing.T) {
	s := store(t)
	owner := me(t)
	if _, err := s.Sync(secrets.SyncParams{Owner: owner, Secrets: []secrets.Secret{{Name: "a", Data: []byte("old")}, {Name: "b", Data: []byte("x")}}}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Sync(secrets.SyncParams{Owner: owner, Secrets: []secrets.Secret{{Name: "a", Data: []byte("new")}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Names, []string{"a"}) {
		t.Fatalf("names = %v", r.Names)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, "a")); string(b) != "new" {
		t.Fatalf("a = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "b")); !os.IsNotExist(err) {
		t.Fatalf("b is still there: %v", err)
	}
	// A temporary file left by an interrupted write goes too.
	if err := os.WriteFile(filepath.Join(s.Dir, "a~new"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if r, err = s.Sync(secrets.SyncParams{Owner: owner, Secrets: []secrets.Secret{{Name: "a", Data: []byte("new")}}}); err != nil || !slices.Equal(r.Names, []string{"a"}) {
		t.Fatalf("names = %v, %v", r.Names, err)
	}
	r, err = s.Sync(secrets.SyncParams{})
	if err != nil || len(r.Names) != 0 {
		t.Fatalf("empty sync = %v, %v", r.Names, err)
	}
}

func TestSyncRefusesBadParams(t *testing.T) {
	s := store(t)
	for _, p := range []secrets.SyncParams{
		{Owner: me(t), Secrets: []secrets.Secret{{Name: "../x"}}},
		{Owner: me(t), Secrets: []secrets.Secret{{Name: ".."}}},
		{Owner: me(t), Secrets: []secrets.Secret{{Name: "a~new"}}},
		{Owner: me(t), Secrets: []secrets.Secret{{Name: "a"}, {Name: "a"}}},
		{Secrets: []secrets.Secret{{Name: "a"}}},
	} {
		_, err := s.Sync(p)
		if e, ok := err.(*proto.Error); !ok || e.Code != proto.CodeInvalidParams {
			t.Errorf("%+v: err = %v", p, err)
		}
	}
	if _, err := s.Sync(secrets.SyncParams{Owner: "no-such-user-glov", Secrets: []secrets.Secret{{Name: "a"}}}); err == nil {
		t.Error("an unknown owner was accepted")
	}
}
