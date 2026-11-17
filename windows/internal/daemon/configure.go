package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/vmname"
)

// configure changes a stopped VM's CPUs, memory or disk size. The disk only
// grows; the guest grows its root file system on the next boot.
func (d *Daemon) configure(caller string, p api.ConfigureParams) (api.VMInfo, error) {
	if p.CPUs == 0 && p.MemoryMiB == 0 && p.DiskGiB == 0 {
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "pass at least one of cpus, memory or disk size")
	}
	switch {
	case p.CPUs < 0 || p.CPUs > runtime.NumCPU():
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "cpus must be 1 to %d (this host's logical processors)", runtime.NumCPU())
	case p.MemoryMiB != 0 && p.MemoryMiB < api.MinMemoryMiB:
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "memory must be at least %d MiB", api.MinMemoryMiB)
	case p.DiskGiB < 0 || p.DiskGiB > api.MaxDiskGiB:
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "disk size must be 1 to %d GiB", api.MaxDiskGiB)
	}
	r, unlock, err := d.locked(caller, p.VM)
	if err != nil {
		return api.VMInfo{}, err
	}
	defer unlock()
	if d.VMs.Running(r.ID) {
		return api.VMInfo{}, fmt.Errorf("VM %s is running: glov vm stop %s first", r.Name, r.Name)
	}
	if p.DiskGiB != 0 && p.DiskGiB < r.DiskGiB {
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "the disk of VM %s is %d GiB and cannot shrink", r.Name, r.DiskGiB)
	}
	if p.DiskGiB > r.DiskGiB {
		if err := d.ResizeDisk(filepath.Join(d.Store.Dir(r.Name), DiskFile), uint64(p.DiskGiB)<<30); err != nil {
			return api.VMInfo{}, fmt.Errorf("grow the disk: %w", err)
		}
		r.DiskGiB = p.DiskGiB
	}
	if p.CPUs != 0 {
		r.CPUs = p.CPUs
	}
	if p.MemoryMiB != 0 {
		r.MemoryMiB = p.MemoryMiB
	}
	if err := d.Store.Save(r); err != nil {
		return api.VMInfo{}, err
	}
	slog.Info("VM configured", "vm", r.Name, "cpus", r.CPUs, "memoryMiB", r.MemoryMiB, "diskGiB", r.DiskGiB)
	return d.info(r), nil
}

// removeGrace bounds how long delete retries the VM's directory while a
// file in it is open. A live share's agent (glov, run by the user) keeps
// its log in the directory until its next look at the VM, every 2 s,
// finds the VM gone; on Windows an open file cannot be deleted.
const removeGrace = 6 * time.Second

// removeDir removes name's directory, retrying while something holds a
// file in it open.
func removeDir(st store.Store, name string) error {
	deadline := time.Now().Add(removeGrace)
	for {
		err := st.Remove(name)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// delete removes a stopped VM and its directory. name must be exact: a
// prefix that happens to match is too easy to get wrong here.
func (d *Daemon) delete(caller, name string) error {
	if err := vmname.Valid(name); err != nil {
		return proto.Errorf(proto.CodeInvalidParams, "%v", err)
	}
	defer d.lock(name)()
	r, err := d.Store.Load(caller, name)
	if errors.Is(err, fs.ErrNotExist) {
		return proto.Errorf(proto.CodeNotFound, "no VM named %s (vm delete needs the exact name)", name)
	}
	if err != nil {
		return err
	}
	if r.ID != "" && d.VMs.Running(r.ID) {
		return fmt.Errorf("VM %s is running: glov vm stop %s first", r.Name, r.Name)
	}
	if r.ID != "" {
		// The base image is shared and stays; drop the deleted VM's grant.
		if err := d.VMs.Release(d.spec(r)); err != nil {
			slog.Warn("revoking the VM's access", "vm", r.Name, "err", err)
		}
	}
	if err := removeDir(d.Store, r.Name); err != nil {
		return err
	}
	d.secretSyncs.Delete(r.Name)
	d.trustSyncs.Delete(r.Name)
	slog.Info("VM deleted", "vm", r.Name)
	return nil
}
