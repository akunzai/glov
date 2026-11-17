// Package secrets delivers the secrets a harness must hold in plain text
// (`secrets.sync`): one file per secret in a tmpfs directory, readable by the
// harness user only, so they never reach the VM's disk.
package secrets

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/akunzai/glov/guest/proto"
)

// Dir is where the files are, a tmpfs of its own.
const Dir = "/run/glov/secrets"

type Secret struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

type SyncParams struct {
	// The guest user that owns the files; required unless Secrets is empty.
	Owner   string   `json:"owner,omitempty"`
	Secrets []Secret `json:"secrets"`
}

type SyncResult struct {
	// The files there now, sorted.
	Names []string `json:"names"`
}

// Store writes into Dir after Prepare, which makes sure Dir is a tmpfs.
type Store struct {
	Dir     string
	Prepare func(dir string) error
}

func New() *Store { return &Store{Dir: Dir, Prepare: prepare} }

// Sync makes the directory hold exactly p.Secrets: each written whole, mode
// 0400 and owned by p.Owner, and every other file removed.
func (s *Store) Sync(p SyncParams) (SyncResult, error) {
	want := map[string]bool{}
	for _, sec := range p.Secrets {
		if err := validName(sec.Name); err != nil {
			return SyncResult{}, err
		}
		if want[sec.Name] {
			return SyncResult{}, proto.Errorf(proto.CodeInvalidParams, "secret %s is given twice", sec.Name)
		}
		want[sec.Name] = true
	}
	var uid, gid int
	if len(p.Secrets) > 0 {
		if p.Owner == "" {
			return SyncResult{}, proto.Errorf(proto.CodeInvalidParams, "owner: name the guest user the secrets are for")
		}
		var err error
		if uid, gid, err = lookup(p.Owner); err != nil {
			return SyncResult{}, err
		}
	}
	if err := s.Prepare(s.Dir); err != nil {
		return SyncResult{}, err
	}
	for _, sec := range p.Secrets {
		if err := s.write(sec, uid, gid); err != nil {
			return SyncResult{}, err
		}
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return SyncResult{}, proto.FromOS(err)
	}
	names := []string{}
	for _, e := range entries {
		if want[e.Name()] {
			names = append(names, e.Name())
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, e.Name())); err != nil {
			return SyncResult{}, proto.FromOS(err)
		}
	}
	slices.Sort(names)
	return SyncResult{Names: names}, nil
}

// write replaces the file by renaming a new one over it, so a reader sees the
// old value or the new one, never part of either.
func (s *Store) write(sec Secret, uid, gid int) error {
	// '~' is in no secret name, so the temporary file never takes one's place.
	tmp := filepath.Join(s.Dir, sec.Name+"~new")
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o400)
	if err != nil {
		return proto.FromOS(err)
	}
	err = f.Chown(uid, gid)
	if err == nil {
		_, err = f.Write(sec.Data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(s.Dir, sec.Name))
	}
	if err != nil {
		_ = os.Remove(tmp)
		return proto.FromOS(err)
	}
	return nil
}

// validName matches the host's secret names: 1-64 of A-Za-z0-9._-, and not
// "." or "..", which name no file.
func validName(name string) error {
	ok := len(name) >= 1 && len(name) <= 64 && name != "." && name != ".."
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			ok = false
		}
	}
	if !ok {
		return proto.Errorf(proto.CodeInvalidParams, "secret name %q: use 1-64 of A-Za-z0-9._-, not . or ..", name)
	}
	return nil
}
