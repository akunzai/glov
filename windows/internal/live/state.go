package live

import (
	"encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Saved is a live share's state between rounds and runs, kept by glov on
// the host: the base and the host's hash cache, for one host folder and
// guest root. A file for another pair is ignored: the base would be wrong.
type Saved struct {
	HostPath  string `json:"hostPath"`
	GuestRoot string `json:"guestRoot"`
	// Disk is the VM's disk the base was taken on: glov vm reset brings
	// back an older guest, against which the base would delete what the
	// host gained since.
	Disk int                `json:"disk,omitzero"`
	Base map[string]saved   `json:"base"`
	Keys map[string]HostKey `json:"keys"`
}

// HostKey is a host file's stat key and the hash it had then.
type HostKey struct {
	Key  string `json:"k"`
	Hash string `json:"h"`
}

type saved struct {
	Dir  bool   `json:"d,omitzero"`
	Hash string `json:"h,omitempty"`
}

// Load reads the state at file for hostPath, guestRoot and disk, empty when
// there is none (or it was for another).
func Load(file, hostPath, guestRoot string, disk int) (Snapshot, map[string]HostKey, error) {
	base, keys := Snapshot{}, map[string]HostKey{}
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return base, keys, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var s Saved
	if json.Unmarshal(data, &s) != nil || !strings.EqualFold(s.HostPath, hostPath) || s.GuestRoot != guestRoot || s.Disk != disk {
		return base, keys, nil
	}
	for p, b := range s.Base {
		base[p] = State{Dir: b.Dir, Hash: b.Hash}
	}
	if s.Keys != nil {
		keys = s.Keys
	}
	return base, keys, nil
}

// Save writes the state to file, replacing it whole.
func Save(file, hostPath, guestRoot string, disk int, base Snapshot, keys map[string]HostKey) error {
	s := Saved{HostPath: hostPath, GuestRoot: guestRoot, Disk: disk, Base: map[string]saved{}, Keys: map[string]HostKey{}}
	for p, st := range base {
		s.Base[p] = saved{st.Dir, st.Hash}
		if k, ok := keys[p]; ok && !st.Dir {
			s.Keys[p] = k // only what is synced: a cache of deleted files would grow
		}
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(file+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(file+".tmp", file)
}
