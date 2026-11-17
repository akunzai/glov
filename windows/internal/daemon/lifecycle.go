package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/progress"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

// Runner runs provisioned VMs; vmrun.Manager is the real one.
type Runner interface {
	// NewID hands out a new VM's ID, which glov-broker ties to the user.
	NewID(ctx context.Context) (string, error)
	Start(spec vmrun.Spec) error
	Adopt(spec vmrun.Spec)
	Stop(ctx context.Context, id string, force bool) error
	Running(id string) bool
	// Info is "running" or "stopped", with the Guest Service while running.
	Info(id string) (string, *api.GuestService)
	WaitGuest(ctx context.Context, id string) error
	// Release undoes what starting the VM granted outside its directory,
	// and gives its ID back, before the VM is deleted.
	Release(spec vmrun.Spec) error
	ExecStart(ctx context.Context, id string, p vmrun.ExecParams) (string, error)
	ExecRead(id, job string, cursor, max int) (vmrun.Read, error)
	ExecKill(ctx context.Context, id, job string) error
	GuestCall(ctx context.Context, id, method string, params, result any) error
	// DialSSH connects to the guest's sshd through its SSH relay.
	DialSSH(ctx context.Context, id string) (io.ReadWriteCloser, error)
	// ExecRunning counts the VM's exec jobs that have not exited.
	ExecRunning(id string) int
	// ExecLost marks the VM's running exec jobs lost.
	ExecLost(id string)
	// Usage measures the running VM on the host.
	Usage(id string) (vmrun.Usage, error)
}

// lock serialises the requests that change the VM called name.
func (d *Daemon) lock(name string) (unlock func()) {
	m, _ := d.locks.LoadOrStore(name, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	return m.(*sync.Mutex).Unlock
}

// spec is what starting r's VM takes.
func (d *Daemon) spec(r store.Record) vmrun.Spec {
	dir := d.Store.Dir(r.Name)
	s := vmrun.Spec{ID: r.ID, Name: r.Name, Dir: dir, Disk: filepath.Join(dir, DiskFile),
		BaseDisk: r.BaseDisk, CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, SecureBoot: api.SecureBoot(r.Distro)}
	owner, name := r.Owner, r.Name
	s.OnReady = func() { d.liveReady(name) }
	s.OnNotify = func(method string, params jsontext.Value) { d.guestNotify(name, method, params) }
	if r.Isolated {
		s.Egress = d.proxy(r)
		s.OnReady = func() {
			d.liveReady(name)
			d.syncSecrets(owner, name)
			d.syncTrust(owner, name)
			d.syncGitSignIns(owner, name)
			d.syncHarnessMode(owner, name, false)
		}
	}
	if r.Template != nil {
		s.Template = filepath.Join(dir, TemplateFile)
	}
	return s
}

// Adopt tracks the owner's VMs that kept running while glovd was down.
func (d *Daemon) Adopt() {
	records, err := d.Store.Records()
	if err != nil {
		slog.Warn("listing VMs", "err", err)
		return
	}
	for _, r := range records {
		if r.Owner == d.Owner && r.ID != "" {
			d.VMs.Adopt(d.spec(r))
		}
	}
}

// info is r's VM's state.
func (d *Daemon) info(r store.Record) api.VMInfo {
	state, gs := d.VMs.Info(r.ID)
	return api.VMInfo{Name: r.Name, State: state, Engine: "hcs", Isolated: r.Isolated, Harnesses: r.Harnesses, Template: r.Template != nil, Disk: r.Disk, Project: r.Project, GuestService: gs}
}

// load resolves ref to one of caller's VMs and reads its record.
func (d *Daemon) load(caller, ref string) (store.Record, error) {
	name, err := d.Store.Resolve(caller, ref)
	if err != nil {
		return store.Record{}, proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	r, err := d.Store.Load(caller, name)
	if err != nil {
		return store.Record{}, proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	return r, nil
}

// locked resolves ref to one of caller's VMs, takes its lock and reads its
// record under the lock, so saving that record keeps every change made
// before. Call unlock when done; it is nil on error.
func (d *Daemon) locked(caller, ref string) (r store.Record, unlock func(), err error) {
	name, err := d.Store.Resolve(caller, ref)
	if err != nil {
		return store.Record{}, nil, proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	unlock = d.lock(name)
	if r, err = d.Store.Load(caller, name); err != nil {
		unlock()
		return store.Record{}, nil, proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	return r, unlock, nil
}

// startable refuses a VM made before every Windows VM was isolated: its
// guest expects a network adapter glovd no longer sets up. Such a VM is not
// migrated.
func startable(r store.Record) error {
	if !r.Isolated {
		return fmt.Errorf("VM %s was created with a network adapter, which glov no longer sets up: every VM on Windows is now isolated (glov vm create --egress open reaches any public host); copy out what you need, then delete it and create it again", r.Name)
	}
	return nil
}

func (d *Daemon) start(caller, ref string) (api.VMInfo, error) {
	r, unlock, err := d.locked(caller, ref)
	if err != nil {
		return api.VMInfo{}, err
	}
	defer unlock()
	if !r.Provisioned || r.ID == "" {
		return api.VMInfo{}, fmt.Errorf("VM %s was never provisioned: delete it and create it again", r.Name)
	}
	if err := startable(r); err != nil {
		return api.VMInfo{}, err
	}
	if d.VMs.Running(r.ID) {
		return api.VMInfo{}, fmt.Errorf("VM %s is already running", r.Name)
	}
	if err := d.VMs.Start(d.spec(r)); err != nil {
		return api.VMInfo{}, err
	}
	return d.info(r), nil
}

// stop stops the VM. A forced stop does not take the VM's lock: it changes
// no record, and must not queue behind a graceful stop or reboot that waits
// on a guest that never shuts down; that one returns once the VM is off.
func (d *Daemon) stop(ctx context.Context, caller string, p api.StopParams) (api.VMInfo, error) {
	var r store.Record
	var err error
	if p.Force {
		r, err = d.load(caller, p.VM)
	} else {
		var unlock func()
		r, unlock, err = d.locked(caller, p.VM)
		if unlock != nil {
			defer unlock()
		}
	}
	if err != nil {
		return api.VMInfo{}, err
	}
	if err := d.VMs.Stop(ctx, r.ID, p.Force); err != nil {
		if errors.Is(err, vmrun.ErrNotRunning) {
			return api.VMInfo{}, fmt.Errorf("VM %s is not running", r.Name)
		}
		return api.VMInfo{}, err
	}
	return d.info(r), nil
}

// reboot stops the VM through the guest and starts it again, returning once
// the new boot's Guest Service answers. Every boot is a new HCS system: the
// VM stops on reset, so a reboot from inside the guest leaves it stopped.
func (d *Daemon) reboot(ctx context.Context, caller string, p api.RebootParams) (api.VMInfo, error) {
	timeout, err := seconds(p.Timeout, api.DefaultRebootTimeout)
	if err != nil {
		return api.VMInfo{}, err
	}
	r, unlock, err := d.locked(caller, p.VM)
	if err != nil {
		return api.VMInfo{}, err
	}
	if err := startable(r); err != nil {
		unlock()
		return api.VMInfo{}, err
	}
	err = d.VMs.Stop(ctx, r.ID, false)
	if err == nil || errors.Is(err, vmrun.ErrNotRunning) {
		err = d.VMs.Start(d.spec(r))
	}
	unlock()
	if err != nil {
		return api.VMInfo{}, err
	}
	return d.wait(ctx, caller, api.WaitParams{VM: r.Name, State: api.WaitGuestReady, Timeout: timeout.Seconds()}, func(string) {})
}

// wait reports what it waits for, then each change of the VM's state and a
// still line every minute, so a long wait does not look hung.
func (d *Daemon) wait(ctx context.Context, caller string, p api.WaitParams, report func(string)) (api.VMInfo, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return api.VMInfo{}, err
	}
	if p.State == "" {
		p.State = api.WaitRunning
	}
	if p.State != api.WaitRunning && p.State != api.WaitGuestReady {
		return api.VMInfo{}, proto.Errorf(proto.CodeInvalidParams, "state must be %q or %q", api.WaitRunning, api.WaitGuestReady)
	}
	timeout, err := seconds(p.Timeout, api.DefaultWaitTimeout)
	if err != nil {
		return api.VMInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	report(fmt.Sprintf("waiting for %s to be %s (up to %s)", r.Name, p.State, progress.Duration(timeout)))
	stop := progress.Poll(func() string { return waitPhase(d.info(r)) }, report, time.Second, time.Minute)
	defer stop()
	if p.State == api.WaitGuestReady {
		if err := d.VMs.WaitGuest(ctx, r.ID); err != nil {
			if errors.Is(err, vmrun.ErrNotRunning) {
				return api.VMInfo{}, fmt.Errorf("VM %s is not running: glov vm start %s", r.Name, r.Name)
			}
			return api.VMInfo{}, fmt.Errorf("the Guest Service of VM %s did not answer within %s", r.Name, timeout)
		}
		return d.info(r), nil
	}
	for !d.VMs.Running(r.ID) {
		select {
		case <-ctx.Done():
			return api.VMInfo{}, fmt.Errorf("VM %s is not running after %s", r.Name, timeout)
		case <-time.After(250 * time.Millisecond):
		}
	}
	return d.info(r), nil
}

// waitPhase is "<vm>: <state>[, Guest Service <state>]".
func waitPhase(v api.VMInfo) string {
	s := v.Name + ": " + v.State
	if v.GuestService != nil {
		s += ", Guest Service " + v.GuestService.State
	}
	return s
}

// seconds turns a timeout parameter into a duration; zero means def.
func seconds(s, def float64) (time.Duration, error) {
	if s == 0 {
		s = def
	}
	if s < 0 || s > 24*3600 {
		return 0, proto.Errorf(proto.CodeInvalidParams, "timeout must be between 0 and 86400 seconds")
	}
	return time.Duration(s * float64(time.Second)), nil
}
