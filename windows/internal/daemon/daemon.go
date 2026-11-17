// Package daemon is glovd's request handling: it answers glov over the
// control pipe for the one user the service was installed for.
package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/secret"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/vmname"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

// Daemon holds what every connection shares.
type Daemon struct {
	// Owner is the SID glov host install recorded; only it is served.
	Owner string
	Store store.Store
	// Cache is the image cache directory, which no share may touch.
	Cache   string
	Version string
	// Prepare returns distro's base image, which a new VM is created from. It may
	// download for minutes; progress receives lines for the user.
	Prepare func(ctx context.Context, distro string, progress func(string)) (Image, error)
	// NewDisk creates disk as a differencing disk on baseDisk, size bytes
	// large; the guest grows its root partition to fill it on first boot.
	NewDisk func(disk, baseDisk string, size uint64) error
	// Provision boots a new VM once to install the Guest Service and
	// returns when the guest has reported and powered off.
	Provision func(ctx context.Context, vm provision.VM, progress func(string)) error
	// ResizeDisk grows a stopped VM's disk to size bytes.
	ResizeDisk func(disk string, size uint64) error
	// MergeDisk writes a differencing disk into its parent (a template).
	MergeDisk func(child string) error
	// VMs runs VMs.
	VMs Runner
	// GuestBinary is the Linux Guest Service glovd installs.
	GuestBinary func() ([]byte, error)
	// HostMemory measures the host for vm.stats.
	HostMemory func() (api.HostMemory, error)

	// Watch paces the watch over a share sync's guest listing; zero means
	// macOS's defaults.
	Watch SyncWatch

	// Secrets holds the secrets isolated VMs' egress proxies put in place
	// of placeholders, and harnesses take in tmpfs.
	Secrets *secret.Store
	// CADir holds the egress CA, made on first use: isolated VMs trust it,
	// and their proxies sign hosts' certificates with it.
	CADir string
	// Quit, if set, stops glovd (daemon.quit); the VMs keep running.
	Quit func()
	// Callers tells who is on a control connection; nil means
	// the named pipes' clients (see hostCallers).
	Callers Callers

	locks sync.Map // VM name -> *sync.Mutex, see lock

	// checks caches vm.status's Guest Service check per VM ID, for one
	// connection.
	checkMu sync.Mutex
	checks  map[string]guestCheck

	syncMu sync.Mutex
	syncs  map[syncKey]*syncState

	tlsMu sync.Mutex
	tls   *egress.TLS

	secretSyncs sync.Map // VM name -> *sync.Mutex, see syncSecrets
	trustSyncs  sync.Map // VM name -> *sync.Mutex, see syncTrust
	gitSyncs    sync.Map // VM name -> *sync.Mutex, see syncGitSignIns
	modeSyncs   sync.Map // VM name -> *sync.Mutex, see syncHarnessMode

	// PruneImages removes the cached base images inUse rejects, except the
	// newest of each distro; nil prunes nothing. busy means a Prepare holds
	// the cache.
	PruneImages func(inUse func(release string) bool) (pruned []api.PrunedImage, busy bool, err error)
	// creating counts createVM calls under way, which may hold a prepared
	// release no record names yet: no image is pruned meanwhile.
	creating atomic.Int32
}

// Image is a prepared base image.
type Image struct {
	// Release names the image within its distro's cache.
	Release string
	// BaseDisk is the base VHDX path.
	BaseDisk string
}

// DiskFile is a VM's disk inside its directory.
const DiskFile = "disk.vhdx"

// methods returns the handlers for one connection from caller, whose SID the
// transport has already checked against d.Owner. Records are still filtered
// by caller, so a record with another owner is never shown or touched. ctx
// ends when the connection does; notify sends a notification on it.
func (d *Daemon) methods(ctx context.Context, caller string, notify func(method string, params any) error) *proto.Server {
	s := proto.NewServer()
	// The share syncs this connection holds end with it, in the guest too:
	// glov share sync was stopped.
	conn := &syncConn{}
	go func() {
		<-ctx.Done()
		d.endSyncs(conn, true, func(syncKey) bool { return true })
	}()
	s.Handle(api.MethodInfo, func(struct{}) (api.Info, error) {
		return api.Info{ProtocolVersion: api.ProtocolVersion, Version: d.Version}, nil
	})
	s.Handle(api.MethodQuit, func(struct{}) (struct{}, error) {
		if d.Quit == nil {
			return struct{}{}, proto.Errorf(proto.CodeInvalidParams, "this glovd cannot be stopped this way")
		}
		slog.Info("glovd asked to quit")
		// Usually after the reply goes out, but the connection can close
		// first; callers treat that as the quit taking effect.
		go d.Quit()
		return struct{}{}, nil
	})
	s.Handle(api.MethodVMList, func(struct{}) ([]api.VMInfo, error) {
		names, err := d.Store.Names(caller)
		if err != nil {
			return nil, err
		}
		vms := make([]api.VMInfo, 0, len(names))
		for _, n := range names {
			if r, err := d.Store.Load(caller, n); err == nil {
				vms = append(vms, d.info(r))
			}
		}
		return vms, nil
	})
	// Async: the first status of a connection checks the Guest Service.
	s.HandleAsync(api.MethodVMStatus, func(p api.VMParams) (api.VMInfo, error) {
		r, err := d.load(caller, p.VM)
		if err != nil {
			return api.VMInfo{}, err
		}
		return d.status(ctx, r), nil
	})
	s.HandleAsync(api.MethodGuestCheck, func(p api.VMParams) (api.GuestCheck, error) {
		return d.guestCheck(ctx, caller, p)
	})
	s.HandleAsync(api.MethodGuestUpdate, func(p api.GuestUpdateParams) (api.GuestUpdated, error) {
		return d.guestUpdate(ctx, caller, p)
	})
	s.HandleAsync(api.MethodVMStats, func(p api.StatsParams) (api.Stats, error) {
		return d.stats(ctx, caller, p)
	})
	s.Handle(api.MethodEgressList, func(p api.EgressParams) (api.Egress, error) {
		return d.egressList(caller, p)
	})
	s.Handle(api.MethodEgressAllow, func(p api.EgressParams) (api.Egress, error) {
		return d.egressEdit(caller, p, true)
	})
	s.Handle(api.MethodEgressRemove, func(p api.EgressParams) (api.Egress, error) {
		return d.egressEdit(caller, p, false)
	})
	s.Handle(api.MethodEgressMCP, func(p api.EgressMCPParams) (api.Egress, error) {
		return d.egressMCP(caller, p)
	})
	s.Handle(api.MethodEgressMode, func(p api.EgressModeParams) (api.Egress, error) {
		return d.egressMode(caller, p)
	})
	s.Handle(api.MethodTrustList, func(p api.TrustParams) (api.Trust, error) {
		return d.trustList(caller, p)
	})
	s.Handle(api.MethodTrustAdd, func(p api.TrustParams) (api.Trust, error) {
		return d.trustEdit(caller, p, true)
	})
	s.Handle(api.MethodTrustRemove, func(p api.TrustParams) (api.Trust, error) {
		return d.trustEdit(caller, p, false)
	})
	s.Handle(api.MethodEgressLog, func(p api.EgressLogParams) (egress.Page, error) {
		return d.egressLog(caller, p)
	})
	s.Handle(api.MethodVMReset, func(p api.VMParams) (api.VMInfo, error) {
		return d.reset(caller, p)
	})
	s.Handle(api.MethodVMFreeze, func(p api.VMParams) (api.VMInfo, error) {
		return d.freeze(caller, p)
	})
	s.Handle(api.MethodVMHarnessAdd, func(p api.HarnessAddParams) (api.HarnessAdded, error) {
		return d.harnessAdd(caller, p)
	})
	s.Handle(api.MethodSecretSet, func(p api.SecretParams) (api.Secret, error) {
		return d.secretSet(caller, p, false)
	})
	s.Handle(api.MethodSecretSource, func(p api.SecretParams) (api.Secret, error) {
		return d.secretSet(caller, p, true)
	})
	s.Handle(api.MethodSecretRefresh, func(p api.SecretParams) (api.Secret, error) {
		return d.secretRefresh(caller, p)
	})
	s.Handle(api.MethodSecretList, func(struct{}) (api.Secrets, error) {
		return d.secretList()
	})
	s.Handle(api.MethodSecretRemove, func(p api.SecretParams) (struct{}, error) {
		return struct{}{}, d.secretRemove(caller, p)
	})
	s.Handle(api.MethodSecretGrant, func(p api.SecretGrantParams) (api.SecretGrants, error) {
		return d.secretGrant(caller, p, true)
	})
	s.Handle(api.MethodSecretRevoke, func(p api.SecretGrantParams) (api.SecretGrants, error) {
		return d.secretGrant(caller, p, false)
	})
	s.Handle(api.MethodSecretGrants, func(p api.SecretGrantParams) (api.SecretGrants, error) {
		return d.secretGrants(caller, p)
	})
	s.Handle(api.MethodSecretRepos, func(p api.SecretReposParams) (api.SecretRepos, error) {
		return d.secretRepos(caller, p)
	})
	s.Handle(api.MethodSecretLogin, func(p api.SecretLoginParams) (api.SecretLogin, error) {
		return d.secretLogin(caller, p)
	})
	s.Handle(api.MethodSecretLogout, func(p api.SecretLoginParams) (api.SecretLogin, error) {
		return d.secretLogout(caller, p)
	})
	s.Handle(api.MethodShareAdd, func(p api.ShareAddParams) (api.ShareInfo, error) {
		return d.shareAdd(caller, p)
	})
	s.Handle(api.MethodShareRemove, func(p api.ShareParams) (struct{}, error) {
		return struct{}{}, d.shareRemove(caller, p)
	})
	s.Handle(api.MethodShareList, func(p api.VMParams) ([]api.ShareInfo, error) {
		return d.shareList(caller, p)
	})
	s.Handle(api.MethodShareSyncBegin, func(p api.ShareSyncBeginParams) (api.ShareSyncBegun, error) {
		return d.syncBegin(conn, caller, p)
	})
	s.HandleAsync(api.MethodShareSyncCall, func(p api.ShareSyncCallParams) (jsontext.Value, error) {
		return d.syncCall(ctx, conn, caller, p)
	})
	s.Handle(api.MethodShareLiveBegin, func(p api.ShareParams) (api.ShareSyncBegun, error) {
		return d.liveBegin(conn, notify, caller, p)
	})
	s.Handle(api.MethodShareLiveConflict, func(p api.ShareLiveConflictParams) (struct{}, error) {
		return struct{}{}, d.liveConflict(conn, caller, p)
	})
	s.Handle(api.MethodShareLiveLocks, func(p api.ShareLiveLocksParams) (struct{}, error) {
		return struct{}{}, d.liveLocks(conn, caller, p)
	})
	s.Handle(api.MethodShareLiveReconcile, func(p api.ShareParams) (struct{}, error) {
		return struct{}{}, d.liveReconcile(caller, p)
	})
	s.Handle(api.MethodShareSyncProgress, func(p api.ShareSyncProgressParams) (struct{}, error) {
		return struct{}{}, d.syncProgress(conn, caller, p)
	})
	s.HandleAsync(api.MethodShareSyncEnd, func(p api.ShareSyncEndParams) (struct{}, error) {
		return struct{}{}, d.syncEnd(conn, caller, p)
	})
	s.HandleAsync(api.MethodShareStatus, func(p api.ShareStatusParams) ([]api.SyncProgress, error) {
		return d.shareStatus(ctx, caller, p)
	})
	// Async, like vm.create below: these can wait for the guest for minutes.
	s.HandleAsync(api.MethodVMStart, func(p api.VMParams) (api.VMInfo, error) {
		return d.start(caller, p.VM)
	})
	s.HandleAsync(api.MethodVMStop, func(p api.StopParams) (api.VMInfo, error) {
		return d.stop(ctx, caller, p)
	})
	s.HandleAsync(api.MethodVMReboot, func(p api.RebootParams) (api.VMInfo, error) {
		return d.reboot(ctx, caller, p)
	})
	s.HandleAsync(api.MethodVMWait, func(p api.WaitParams) (api.VMInfo, error) {
		return d.wait(ctx, caller, p, func(msg string) {
			notify(api.NotifyProgress, api.Progress{Message: msg})
		})
	})
	s.Handle(api.MethodVMConfigure, func(p api.ConfigureParams) (api.VMInfo, error) {
		return d.configure(caller, p)
	})
	s.Handle(api.MethodVMDelete, func(p api.VMParams) (api.VMDeleted, error) {
		if err := d.delete(caller, p.VM); err != nil {
			return api.VMDeleted{}, err
		}
		return api.VMDeleted{PrunedImages: d.pruneImages()}, nil
	})
	s.HandleAsync(api.MethodExecStart, func(p api.ExecParams) (api.JobStarted, error) {
		return d.execStart(ctx, caller, p)
	})
	s.Handle(api.MethodVMConsole, func(p api.ConsoleParams) (api.ConsoleRead, error) {
		return d.console(caller, p)
	})
	s.Handle(api.MethodExecRead, func(p api.ExecReadParams) (api.ExecRead, error) {
		return d.execRead(caller, p)
	})
	s.HandleAsync(api.MethodExecKill, func(p api.ExecKillParams) (struct{}, error) {
		return struct{}{}, d.execKill(ctx, caller, p)
	})
	s.HandleAsync(api.MethodGuestCall, func(p api.GuestCallParams) (jsontext.Value, error) {
		return d.guestCall(ctx, caller, p)
	})
	// Async: the connection keeps being read, so glov going away (Ctrl+C)
	// ends ctx and stops a download.
	s.HandleAsync(api.MethodVMCreate, func(p api.CreateParams) (api.VMInfo, error) {
		return d.create(ctx, caller, p, func(msg string) {
			notify(api.NotifyProgress, api.Progress{Message: msg})
		})
	})
	return s
}

func (d *Daemon) create(ctx context.Context, caller string, p api.CreateParams, progress func(string)) (api.VMInfo, error) {
	r, err := d.createVM(ctx, caller, p, progress)
	if err != nil {
		return api.VMInfo{}, err
	}
	return d.info(r), nil
}

// createVM creates and provisions a VM. Then it prunes the base images no VM uses, such as the one a newer build
// just replaced.
func (d *Daemon) createVM(ctx context.Context, caller string, p api.CreateParams, progress func(string)) (store.Record, error) {
	d.creating.Add(1)
	r, err := d.newVM(ctx, caller, p, progress)
	d.creating.Add(-1)
	if err == nil {
		for _, im := range d.pruneImages() {
			progress(im.Describe())
		}
	}
	return r, err
}

// pruneImages prunes the base images that no VM's record names, while no
// VM is being created, and logs what it removed.
func (d *Daemon) pruneImages() []api.PrunedImage {
	if d.PruneImages == nil {
		return nil
	}
	// Fail closed: a VM whose record cannot be read, or that names no base
	// image, may still need any of them.
	recs, err := d.Store.AllRecords()
	if err != nil {
		slog.Warn("base images not pruned: reading the VMs", "err", err)
		return nil
	}
	used := map[string]bool{}
	for _, r := range recs {
		if r.Release == "" || r.BaseDisk == "" {
			slog.Warn("base images not pruned: a VM names no base image", "vm", r.Name)
			return nil
		}
		// The differencing disk names its parent by path: keep that
		// directory too, should it differ from the release.
		used[r.Release], used[filepath.Base(filepath.Dir(r.BaseDisk))] = true, true
	}
	// Called under the cache's lock: a create that started since the
	// records were read waits for it in Prepare, or is counted here.
	pruned, busy, err := d.PruneImages(func(release string) bool { return used[release] || d.creating.Load() > 0 })
	for _, im := range pruned {
		slog.Info("base image pruned", "release", im.Release, "bytes", im.Bytes)
	}
	if err != nil {
		slog.Warn("pruning base images", "err", err)
	}
	if busy {
		slog.Info("base images not pruned: a download runs")
	}
	return pruned
}

func (d *Daemon) newVM(ctx context.Context, caller string, p api.CreateParams, progress func(string)) (store.Record, error) {
	if err := createDefaults(&p); err != nil {
		return store.Record{}, err
	}
	// Fail before a long download; Store.Create checks again atomically.
	if _, err := os.Stat(filepath.Join(d.Store.Dir(p.VM), "vm.json")); !errors.Is(err, fs.ErrNotExist) {
		return store.Record{}, proto.Errorf(proto.CodeInvalidParams, "a VM named %s exists", p.VM)
	}
	img, err := d.Prepare(ctx, p.Distro, progress)
	if err != nil {
		return store.Record{}, err
	}
	if err := ctx.Err(); err != nil {
		return store.Record{}, err
	}
	id, err := d.VMs.NewID(ctx)
	if err != nil {
		return store.Record{}, err
	}
	rec := store.Record{
		Name: p.VM, ID: id, Owner: caller, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB, DiskGiB: p.DiskGiB,
		Distro: p.Distro, Release: img.Release, BaseDisk: img.BaseDisk, Isolated: p.Isolated,
		Project: p.Project,
	}
	if p.Isolated {
		rec.Egress = &store.Egress{Allow: []string{}, Open: p.Open}
	}
	harnesses, _ := harness.Resolve(p.Harnesses) // createDefaults checked them
	for _, h := range harnesses {
		rec.Harnesses = append(rec.Harnesses, h.Name)
		for _, host := range h.Runtime {
			if !slices.Contains(rec.Egress.Allow, host) {
				rec.Egress.Allow = append(rec.Egress.Allow, host)
			}
		}
	}
	var ca []byte
	if p.Isolated {
		t, err := d.egressTLS()
		if err != nil {
			return store.Record{}, fmt.Errorf("egress CA: %v", err)
		}
		ca = t.CA.PEM
	}
	// Under the name's lock: a delete of the same name may still be
	// removing the old folder (removeDir), whose vm.json is already gone,
	// and would remove this VM's files with it.
	unlock := d.lock(p.VM)
	err = d.Store.Create(rec)
	unlock()
	if errors.Is(err, store.ErrExists) {
		return store.Record{}, proto.Errorf(proto.CodeInvalidParams, "a VM named %s exists", p.VM)
	}
	if err != nil {
		return store.Record{}, err
	}
	dir := d.Store.Dir(p.VM)
	disk := filepath.Join(dir, DiskFile)
	// undo removes a VM that never provisioned, and what it was granted.
	undo := func() {
		if err := d.VMs.Release(vmrun.Spec{ID: rec.ID, Name: p.VM, BaseDisk: img.BaseDisk}); err != nil {
			slog.Warn("releasing a VM that was not created", "vm", p.VM, "err", err)
		}
		d.Store.Remove(p.VM)
	}
	if err := d.NewDisk(disk, img.BaseDisk, uint64(p.DiskGiB)<<30); err != nil {
		undo()
		return store.Record{}, err
	}
	// A VM that never provisioned has no Guest Service and cannot be
	// repaired from the host: undo it, and the user creates it again.
	err = d.Provision(ctx, provision.VM{
		ID: rec.ID, Name: p.VM, Distro: p.Distro, Dir: dir, Disk: disk, BaseDisk: img.BaseDisk, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB,
		Isolated: p.Isolated, Harnesses: harnesses, EgressCA: ca,
	}, func(step string) { progress("provisioning: " + step) })
	if err == nil {
		rec.Provisioned = true
		err = d.Store.Save(rec)
	}
	if err != nil {
		undo()
		return store.Record{}, err
	}
	slog.Info("VM created", "vm", p.VM, "isolated", p.Isolated, "harnesses", rec.Harnesses, "distro", p.Distro, "release", img.Release, "cpus", p.CPUs, "memoryMiB", p.MemoryMiB, "diskGiB", p.DiskGiB)
	return rec, nil
}

// createDefaults fills p's zero fields and checks it.
func createDefaults(p *api.CreateParams) error {
	if err := vmname.Valid(p.VM); err != nil {
		return proto.Errorf(proto.CodeInvalidParams, "%v", err)
	}
	// Every Windows VM is isolated: no network adapter, so glovd never
	// touches host networking (which a non-elevated glovd could not).
	p.Isolated = true
	if p.CPUs == 0 {
		p.CPUs = min(api.DefaultCPUs, runtime.NumCPU())
	}
	if p.MemoryMiB == 0 {
		p.MemoryMiB = api.DefaultMemoryMiB
	}
	if p.DiskGiB == 0 {
		p.DiskGiB = api.DefaultDiskGiB
	}
	if p.Distro == "" {
		p.Distro = api.Distros[0]
	}
	if !slices.Contains(api.Distros, p.Distro) {
		return proto.Errorf(proto.CodeInvalidParams, "unknown distro %q: want one of %s", p.Distro, strings.Join(api.Distros, ", "))
	}
	if _, err := harness.Resolve(p.Harnesses); err != nil {
		return proto.Errorf(proto.CodeInvalidParams, "%v", err)
	}
	if pr := p.Project; pr != nil {
		if (pr.Sync != api.SyncGit && pr.Sync != api.SyncLive) || !filepath.IsAbs(pr.Path) {
			return proto.Errorf(proto.CodeInvalidParams, "project %+v: want an absolute path and sync %s or %s", *pr, api.SyncGit, api.SyncLive)
		}
	}
	switch {
	case p.CPUs < 1 || p.CPUs > runtime.NumCPU():
		return proto.Errorf(proto.CodeInvalidParams, "cpus must be 1 to %d (this host's logical processors)", runtime.NumCPU())
	case p.MemoryMiB < api.MinMemoryMiB:
		return proto.Errorf(proto.CodeInvalidParams, "memory must be at least %d MiB", api.MinMemoryMiB)
	case p.DiskGiB < 1 || p.DiskGiB > api.MaxDiskGiB:
		return proto.Errorf(proto.CodeInvalidParams, "disk size must be 1 to %d GiB", api.MaxDiskGiB)
	}
	return nil
}
