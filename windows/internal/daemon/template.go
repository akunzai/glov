package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/store"
)

// TemplateFile is a frozen VM's template disk inside its directory: the
// disk as glov vm harness install left it, which DiskFile, a differencing
// disk on it, starts from again on every glov vm reset. The chain is the
// cache's base disk, then TemplateFile, then DiskFile.
const TemplateFile = "template.vhdx"

// cloneEgress is a deep copy, so a template's recorded settings never share
// a slice or map with the live ones.
func cloneEgress(e *store.Egress) store.Egress {
	if e == nil {
		return store.Egress{Allow: []string{}}
	}
	c := *e
	c.Allow = slices.Clone(e.Allow)
	if c.Allow == nil {
		c.Allow = []string{}
	}
	c.Secrets = slices.Clone(e.Secrets)
	c.PrivateAllow = slices.Clone(e.PrivateAllow)
	c.Trust = slices.Clone(e.Trust)
	c.PlainSecrets = slices.Clone(e.PlainSecrets)
	c.Repos = slices.Clone(e.Repos)
	c.Subscriptions = slices.Clone(e.Subscriptions)
	return c
}

// stoppedIsolated loads caller's VM ref under its lock, which the caller
// releases: an isolated VM that is not running.
func (d *Daemon) stoppedIsolated(caller, ref, verb string) (store.Record, func(), error) {
	r, unlock, err := d.lockedIsolated(caller, ref)
	if err != nil {
		return store.Record{}, nil, err
	}
	if r.ID != "" && d.VMs.Running(r.ID) {
		unlock()
		return store.Record{}, nil, fmt.Errorf("VM %s is running: glov vm stop %s first, then %s", r.Name, r.Name, verb)
	}
	return r, unlock, nil
}

// freeze makes the stopped VM's disk its template, with the egress
// settings of the moment, and starts a fresh child disk on it. A VM that
// is already frozen merges its child into the template first, so the
// template takes what the child holds (glov vm harness install again).
func (d *Daemon) freeze(caller string, p api.VMParams) (api.VMInfo, error) {
	r, unlock, err := d.stoppedIsolated(caller, p.VM, "glov vm harness install")
	if err != nil {
		return api.VMInfo{}, err
	}
	defer unlock()
	if r.Project != nil {
		// Its work is on the disk: a reset would lose it.
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "VM %s is the VM of %s, which is never frozen", r.Name, r.Project.Path)
	}
	dir := d.Store.Dir(r.Name)
	disk, template := filepath.Join(dir, DiskFile), filepath.Join(dir, TemplateFile)
	size := uint64(r.DiskGiB) << 30
	if r.Template == nil {
		// A template left by a freeze that failed before saving the record
		// would be overwritten by the rename; refuse rather than guess.
		if _, err := os.Stat(template); !errors.Is(err, fs.ErrNotExist) {
			return api.VMInfo{}, fmt.Errorf("VM %s has a %s but no template in vm.json: remove the file, or delete the VM", r.Name, TemplateFile)
		}
		if err := os.Rename(disk, template); err != nil {
			return api.VMInfo{}, err
		}
		if err := d.NewDisk(disk, template, size); err != nil {
			os.Rename(template, disk)
			return api.VMInfo{}, fmt.Errorf("start a disk on the template: %w", err)
		}
	} else {
		// The child may have grown (glov vm configure); the template must
		// be as large to take it.
		if err := d.ResizeDisk(template, size); err != nil {
			return api.VMInfo{}, fmt.Errorf("grow the template: %w", err)
		}
		if err := d.MergeDisk(disk); err != nil {
			return api.VMInfo{}, fmt.Errorf("merge the disk into the template: %w", err)
		}
		if err := d.newChild(disk, template, size); err != nil {
			return api.VMInfo{}, err
		}
	}
	r.Template = &store.Template{Egress: cloneEgress(r.Egress), Frozen: time.Now().UTC().Format(time.RFC3339)}
	if err := d.Store.Save(r); err != nil {
		return api.VMInfo{}, err
	}
	slog.Info("VM frozen as its template", "vm", r.Name)
	return d.info(r), nil
}

// reset drops the stopped, frozen VM's child disk for a fresh one on the
// template and restores the template's egress settings. Shares stay; what
// was synced into the guest is gone with the disk.
func (d *Daemon) reset(caller string, p api.VMParams) (api.VMInfo, error) {
	r, unlock, err := d.stoppedIsolated(caller, p.VM, "glov vm reset")
	if err != nil {
		return api.VMInfo{}, err
	}
	defer unlock()
	if r.Template == nil {
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "VM %s has no template: glov vm harness install %s makes one", r.Name, r.Name)
	}
	dir := d.Store.Dir(r.Name)
	if err := d.newChild(filepath.Join(dir, DiskFile), filepath.Join(dir, TemplateFile), uint64(r.DiskGiB)<<30); err != nil {
		return api.VMInfo{}, err
	}
	r.Disk++
	e := cloneEgress(&r.Template.Egress)
	// A secret revoked since the freeze stays revoked.
	var current []string
	if r.Egress != nil {
		current = r.Egress.Secrets
	}
	e.Secrets = slices.DeleteFunc(e.Secrets, func(n string) bool { return !slices.Contains(current, n) })
	if len(e.Secrets) == 0 {
		e.Secrets = nil
	}
	// Likewise a VM restricted since stays restricted, and a tmpfs secret
	// the owner no longer lets it hold while open stays withheld.
	e.Open = e.Open && r.Egress != nil && r.Egress.Open
	if !e.Open {
		e.PlainSecrets = nil
	}
	// A certificate removed since stays removed: the template disk still
	// holds it, and the next trust sync takes it out of the guest.
	// A repository removed since stays out of reach too.
	var trusted []egress.Trust
	var repos []string
	if r.Egress != nil {
		e.PlainSecrets = slices.DeleteFunc(e.PlainSecrets, func(n string) bool { return !slices.Contains(r.Egress.PlainSecrets, n) })
		trusted, repos = r.Egress.Trust, r.Egress.Repos
	}
	e.Trust = slices.DeleteFunc(e.Trust, func(t egress.Trust) bool { return !slices.Contains(trusted, t) })
	if len(e.Trust) == 0 {
		e.Trust = nil
	}
	e.Repos = slices.DeleteFunc(e.Repos, func(p string) bool { return !slices.Contains(repos, p) })
	if len(e.Repos) == 0 {
		e.Repos = nil
	}
	if len(e.PlainSecrets) == 0 {
		e.PlainSecrets = nil
	}
	// The subscriptions are vm.json's as they are now, with their tokens'
	// grants and runtime hosts: signing in and out is the owner's on the
	// host, so a template frozen before glov secret login keeps it, and one
	// frozen before a logout does not undo it. The guest is switched to
	// match at its next Guest Service session.
	e.Subscriptions = nil
	if r.Egress != nil {
		e.Subscriptions = slices.Clone(r.Egress.Subscriptions)
	}
	for _, h := range e.Subscriptions {
		prof, err := namedProfile(h)
		if err != nil || prof.Subscription == nil {
			continue
		}
		for _, n := range subscriptionNames(r.Name, prof) {
			if slices.Contains(current, n) && !slices.Contains(e.Secrets, n) {
				e.Secrets = append(e.Secrets, n)
			}
		}
		for _, host := range prof.Subscription.Runtime {
			if !slices.Contains(e.Allow, host) {
				e.Allow = append(e.Allow, host)
			}
		}
	}
	r.Egress = &e
	if err := d.Store.Save(r); err != nil {
		return api.VMInfo{}, err
	}
	slog.Info("VM reset to its template", "vm", r.Name)
	return d.info(r), nil
}

// newChild replaces disk with an empty differencing disk on template.
func (d *Daemon) newChild(disk, template string, size uint64) error {
	if err := os.Remove(disk); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := d.NewDisk(disk, template, size); err != nil {
		return fmt.Errorf("start a disk on the template: %w (the VM has no disk until glov vm reset succeeds)", err)
	}
	return nil
}
