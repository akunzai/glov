package broker

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
)

// Owners records which user each VM ID was handed to, in a JSON file in the
// broker's data directory. Access to a VM's disks hangs on its ID (the VM
// worker runs as an account derived from it), so an ID is never handed to
// two users, and survives the broker's restarts until its VM is deleted.
type Owners struct {
	Path string

	mu  sync.Mutex
	ids map[string]string // VM ID (lower case) -> owner SID; nil until loaded
}

// ErrNotOwner is returned for an ID the broker did not hand the caller.
var ErrNotOwner = errors.New("not a VM ID this user was given")

func (o *Owners) load() error {
	if o.ids != nil {
		return nil
	}
	b, err := os.ReadFile(o.Path)
	if errors.Is(err, fs.ErrNotExist) {
		o.ids = map[string]string{}
		return nil
	}
	if err != nil {
		return err
	}
	var ids map[string]string
	if err := json.Unmarshal(b, &ids); err != nil {
		return fmt.Errorf("%s: %w", o.Path, err)
	}
	if ids == nil {
		ids = map[string]string{}
	}
	o.ids = ids
	return nil
}

func (o *Owners) save(ids map[string]string) error {
	b, err := json.Marshal(ids, json.Deterministic(true))
	if err != nil {
		return err
	}
	tmp := o.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, o.Path)
}

// Add records id as sid's. It fails if id is taken.
func (o *Owners) Add(id, sid string) error {
	id = strings.ToLower(id)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.load(); err != nil {
		return err
	}
	if _, ok := o.ids[id]; ok {
		return fmt.Errorf("VM ID %s is taken", id)
	}
	next := map[string]string{id: sid}
	for k, v := range o.ids {
		next[k] = v
	}
	if err := o.save(next); err != nil {
		return err
	}
	o.ids = next
	return nil
}

// Check fails with ErrNotOwner unless id is sid's.
func (o *Owners) Check(id, sid string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.load(); err != nil {
		return err
	}
	if owner, ok := o.ids[strings.ToLower(id)]; !ok || owner != sid {
		return ErrNotOwner
	}
	return nil
}

// Remove forgets id.
func (o *Owners) Remove(id string) error {
	id = strings.ToLower(id)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.load(); err != nil {
		return err
	}
	if _, ok := o.ids[id]; !ok {
		return nil
	}
	next := map[string]string{}
	for k, v := range o.ids {
		if k != id {
			next[k] = v
		}
	}
	if err := o.save(next); err != nil {
		return err
	}
	o.ids = next
	return nil
}

// IDs lists the IDs handed to sid, or every ID for sid "".
func (o *Owners) IDs(sid string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.load(); err != nil {
		return nil, err
	}
	var ids []string
	for id, owner := range o.ids {
		if sid == "" || owner == sid {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}
