// Package store keeps glovd's VM records: one directory per VM under
// <data>\vms, holding vm.json. Every record names the SID of the user who owns
// the VM, and every lookup is filtered by it.
package store

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/share"
	"github.com/akunzai/glov/windows/internal/vmname"
)

// Record is a VM's vm.json.
type Record struct {
	Name string `json:"name"`
	// ID is the VM's GUID: its HCS compute system ID, from which the SID
	// its worker process runs as, and is granted its disks to, derives.
	ID string `json:"id"`
	// Owner is the SID of the user who created the VM.
	Owner     string `json:"owner"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memoryMiB"`
	DiskGiB   int    `json:"diskGiB"`
	// Distro is the Linux distribution the VM runs, such as "debian".
	Distro string `json:"distro"`
	// Release is the release the VM's disk is based on (a directory
	// in glovd's image cache).
	Release string `json:"release"`
	// BaseDisk is the full path of the base image the VM's differencing
	// disk names.
	BaseDisk string `json:"baseDisk"`
	// Provisioned is set once the VM's first boot has installed the Guest
	// Service and grown its root partition to DiskGiB.
	Provisioned bool `json:"provisioned"`
	// Shares are the host folders glov share sync mirrors into the guest,
	// which are also the VM's copy allowlist.
	Shares []share.Share `json:"shares,omitempty"`
	// Isolated VMs have no network adapter: the guest gets out only
	// through the egress proxy, to what Egress allows. Set at creation
	// only, as on macOS.
	Isolated bool `json:"isolated,omitzero"`
	// Egress is an isolated VM's egress policy, re-read for every
	// connection; nil allows nothing.
	Egress *Egress `json:"egress,omitempty"`
	// Harnesses are the harness profiles it was created with, as on macOS.
	Harnesses []string `json:"harnesses,omitempty"`
	// Template is set once glov vm harness install froze the VM's disk: glov
	// vm reset starts it from there again.
	Template *Template `json:"template,omitempty"`
	// Disk counts the disks glov vm reset gave the VM: a live share keeps
	// in step against a base only of the disk it was taken on.
	Disk int `json:"disk,omitzero"`
	// Project is set on the VM of a project (glov claude): its work lives
	// on its disk, so it is never frozen or reset.
	Project *api.Project `json:"project,omitempty"`
}

// Template is a frozen VM's template: the egress settings recorded with
// its disk, which glov vm reset restores.
type Template struct {
	Egress Egress `json:"egress"`
	// Frozen is when, in RFC 3339 UTC.
	Frozen string `json:"frozen"`
}

// Egress is an isolated VM's egress settings, with macOS vm.json's keys.
type Egress = egress.Settings

// ErrExists reports a name already taken.
var ErrExists = errors.New("store: VM exists")

// Store is rooted at glovd's data directory.
type Store struct{ Root string }

// Dir is the directory of the VM called name; name must be valid.
func (s Store) Dir(name string) string { return filepath.Join(s.Root, "vms", name) }

// Names lists owner's VMs, sorted. Directories with an invalid name, no
// readable vm.json or another owner are left out.
func (s Store) Names(owner string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "vms"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || vmname.Valid(e.Name()) != nil {
			continue
		}
		if r, err := s.read(e.Name()); err == nil && r.Owner == owner {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Records returns every VM record, of any owner, sorted by name; unreadable
// ones are left out. Only glov host install uses it, to check the VMs on
// this machine before it moves the image cache.
func (s Store) Records() ([]Record, error) { return s.records(false) }

// AllRecords is Records, but fails on a VM directory whose record cannot
// be read, for a caller that must not miss a VM (pruning base images).
func (s Store) AllRecords() ([]Record, error) { return s.records(true) }

func (s Store) records(strict bool) ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "vms"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, e := range entries {
		if !e.IsDir() || vmname.Valid(e.Name()) != nil {
			continue
		}
		r, err := s.read(e.Name())
		if err != nil {
			if _, serr := os.Lstat(filepath.Join(s.Dir(e.Name()), "vm.json")); errors.Is(serr, fs.ErrNotExist) {
				continue // no VM, only glov's live share state
			}
			if strict {
				return nil, fmt.Errorf("VM %s: %w", e.Name(), err)
			}
			continue
		}
		records = append(records, r)
	}
	return records, nil
}

// Resolve maps a name or unique prefix to one of owner's VMs.
func (s Store) Resolve(owner, ref string) (string, error) {
	names, err := s.Names(owner)
	if err != nil {
		return "", err
	}
	return vmname.Resolve(ref, names)
}

// Load reads owner's VM called name. Another owner's VM reads as missing.
func (s Store) Load(owner, name string) (Record, error) {
	if err := vmname.Valid(name); err != nil {
		return Record{}, err
	}
	r, err := s.read(name)
	if err != nil {
		return Record{}, err
	}
	if r.Owner != owner {
		return Record{}, fmt.Errorf("no VM named %q: %w", name, fs.ErrNotExist)
	}
	return r, nil
}

func (s Store) read(name string) (Record, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir(name), "vm.json"))
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("%s: %w", name, err)
	}
	if r.Name != name {
		return Record{}, fmt.Errorf("%s: vm.json names %q", name, r.Name)
	}
	return r, nil
}

// Remove deletes the VM called name, directory and all. It is for undoing a
// Create that failed halfway; owner checks are the caller's.
func (s Store) Remove(name string) error {
	if err := vmname.Valid(name); err != nil {
		return err
	}
	return os.RemoveAll(s.Dir(name))
}

// Create writes a new VM's directory and vm.json. A name taken by any owner
// is refused. A directory without vm.json is no VM: glov keeps a VM's
// live share state in the same folder, which can outlast the VM.
func (s Store) Create(r Record) error {
	if err := vmname.Valid(r.Name); err != nil {
		return err
	}
	if r.Owner == "" {
		return errors.New("store: record without an owner")
	}
	if err := os.MkdirAll(filepath.Join(s.Root, "vms"), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(s.Dir(r.Name), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	// Claim the name: of two creates, only one makes vm.json.
	f, err := os.OpenFile(filepath.Join(s.Dir(r.Name), "vm.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w: %s", ErrExists, r.Name)
	}
	if err != nil {
		return err
	}
	f.Close()
	return s.write(r)
}

// Save replaces the vm.json of the existing VM r.Name; owner checks are the
// caller's.
func (s Store) Save(r Record) error {
	if err := vmname.Valid(r.Name); err != nil {
		return err
	}
	if _, err := s.read(r.Name); err != nil {
		return err
	}
	return s.write(r)
}

// write replaces vm.json through a rename, so a crash leaves the old or the
// new record, never half of one.
func (s Store) write(r Record) error {
	data, err := json.Marshal(r, json.Deterministic(true))
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir(r.Name), "vm.json")
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	// Go opens files without FILE_SHARE_DELETE, so the rename fails while
	// another goroutine reads vm.json, as an isolated VM's egress proxy
	// does for every connection; such a read is over in moments.
	for range renameTries - 1 {
		if err := os.Rename(path+".tmp", path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return os.Rename(path+".tmp", path)
}

// renameTries bounds write's rename attempts, 10 ms apart.
const renameTries = 100
