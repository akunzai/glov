// Package vmrun runs provisioned VMs through glov-broker and keeps a
// session with each one's Guest Service. A VM outlives glovd: the broker
// keeps it running when glovd's connection ends, and Adopt finds it again
// by its ID.
package vmrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/hcs"
	"github.com/akunzai/glov/windows/internal/rpc"
)

// pollInterval is how often a running VM's state is checked.
const pollInterval = time.Second

// Host is what Manager asks of glov-broker; *broker.Conn is the real one.
type Host interface {
	NewID(ctx context.Context) (string, error)
	ReleaseID(ctx context.Context, id string) error
	Create(ctx context.Context, p broker.CreateParams) error
	Start(ctx context.Context, id string) error
	State(ctx context.Context, id string) (string, error)
	Shutdown(ctx context.Context, id string) error
	Terminate(ctx context.Context, id string) error
	Stats(ctx context.Context, id string) (broker.Stats, error)
}

// Manager tracks the VMs glovd runs.
type Manager struct {
	host Host
	mu   sync.Mutex
	vms  map[string]*running // by ID
	jobs map[string]*jobs    // by ID, kept after a VM stops so its jobs can be read
	// stopTimeout is StopTimeout, shortened by tests.
	stopTimeout time.Duration
}

// NewManager returns a Manager with no VMs, which runs them through host.
func NewManager(host Host) *Manager {
	return &Manager{host: host, vms: map[string]*running{}, jobs: map[string]*jobs{}, stopTimeout: StopTimeout}
}

// system is what Stop and Usage ask of a running VM's compute system;
// brokerVM is the real one.
type system interface {
	Shutdown() error
	Terminate() error
	Stats() (broker.Stats, error)
}

// brokerVM is a VM's compute system, through glov-broker.
type brokerVM struct {
	host Host
	id   string
}

// callTimeout bounds a broker call that does not wait on the guest.
const callTimeout = 30 * time.Second

func callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

// Shutdown returns once the guest has taken the request, which can take
// long; Stop bounds it.
func (v brokerVM) Shutdown() error { return v.host.Shutdown(context.Background(), v.id) }

func (v brokerVM) Terminate() error {
	ctx, cancel := callContext()
	defer cancel()
	return v.host.Terminate(ctx, v.id)
}

func (v brokerVM) Stats() (broker.Stats, error) {
	ctx, cancel := callContext()
	defer cancel()
	return v.host.Stats(ctx, v.id)
}

func (v brokerVM) State() (string, error) {
	ctx, cancel := callContext()
	defer cancel()
	return v.host.State(ctx, v.id)
}

// NewID asks the broker for a new VM's ID.
func (m *Manager) NewID(ctx context.Context) (string, error) { return m.host.NewID(ctx) }

type running struct {
	spec    Spec
	sys     system
	session *session
	exited  chan struct{} // closed once HCS no longer runs the VM
	stopped chan struct{} // closed once glovd has cleaned up after it
	cancel  context.CancelFunc
}

// Start boots the VM. It returns once HCS has started it; the Guest Service
// comes up seconds later (WaitGuest).
func (m *Manager) Start(spec Spec) error {
	id, err := guid.FromString(spec.ID)
	if err != nil {
		return fmt.Errorf("VM ID: %w", err)
	}
	if spec.Egress == nil {
		return fmt.Errorf("VM %s has no egress proxy: every VM on Windows is isolated", spec.Name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vms[spec.ID]; ok {
		return fmt.Errorf("VM %s is already running", spec.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sys := brokerVM{m.host, spec.ID}
	// The VM's worker process opens its disks as the VM's own account.
	// glovd may have reset the data directory's ACLs since, so grant
	// again on every start. The grant is glovd's, made as the user, so it
	// reaches only files the user may change.
	for _, p := range []string{spec.Disk, spec.Template, spec.BaseDisk} {
		if p == "" {
			continue
		}
		if err := hcs.GrantVmAccess(spec.ID, p); err != nil {
			return fmt.Errorf("grant the VM %s: %w", p, err)
		}
	}
	ln, err := broker.ListenConsole(spec.ID)
	if err != nil {
		return fmt.Errorf("console pipe: %w", err)
	}
	// Accepting before Create: the VM's worker connects to COM1 once, as
	// the system is created, and a pipe not yet accepting answers busy.
	go logConsole(ln, filepath.Join(spec.Dir, ConsoleLog))
	err = m.host.Create(ctx, broker.CreateParams{ID: spec.ID, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB,
		Disk: spec.Disk, SecureBoot: spec.SecureBoot, KeepRunning: true})
	if errors.Is(err, broker.ErrAlreadyRunning) {
		// Running unseen, as after glovd restarted: adopted rather than
		// started twice.
		ln.Close()
		m.adopt(spec, id, sys)
		return fmt.Errorf("VM %s is already running", spec.Name)
	}
	if err != nil {
		ln.Close()
		return fmt.Errorf("create the VM: %w", err)
	}
	// The egress listener can be bound only once the system exists, and
	// must be before the guest boots.
	egressLn, err := listenEgress(id)
	if err != nil {
		sys.Terminate()
		ln.Close()
		return fmt.Errorf("egress proxy: %w", err)
	}
	if err := m.host.Start(ctx, spec.ID); err != nil {
		egressLn.Close()
		sys.Terminate()
		ln.Close()
		return fmt.Errorf("start the VM: %w", err)
	}
	m.track(spec, id, sys, egressLn)
	// The pipe's name stays taken while ln is open, and the next start
	// needs it: a worker that never connected must not hold it.
	r := m.vms[spec.ID]
	go func() { <-r.exited; ln.Close() }()
	slog.Info("VM started", "vm", spec.Name)
	return nil
}

// listenEgress binds the VM's egress port. Only the VM can connect to it:
// the listener names the VM's ID.
func listenEgress(id guid.GUID) (net.Listener, error) {
	l, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: id, ServiceID: winio.VsockServiceID(egress.Port)})
	if err != nil {
		return nil, err // not a nil *HvsockListener in a non-nil interface
	}
	return l, nil
}

// adopt tracks spec's running system sys; m.mu is held. A VM made before
// every Windows VM was isolated has no egress proxy and keeps running
// without one until it stops.
func (m *Manager) adopt(spec Spec, id guid.GUID, sys brokerVM) {
	var l net.Listener
	if spec.Egress != nil {
		var err error
		if l, err = listenEgress(id); err != nil {
			slog.Warn("VM found running without its egress proxy", "vm", spec.Name, "err", err)
		}
	}
	m.track(spec, id, sys, l)
}

// Adopt tracks spec's VM if it is running, as after glovd restarts.
func (m *Manager) Adopt(spec Spec) {
	id, err := guid.FromString(spec.ID)
	if err != nil {
		return
	}
	sys := brokerVM{m.host, spec.ID}
	state, err := sys.State()
	if err != nil {
		slog.Warn("VM not adopted", "vm", spec.Name, "err", err)
		return
	}
	if state == "" {
		return
	}
	if state != broker.Running {
		sys.Terminate()
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vms[spec.ID]; ok {
		return
	}
	m.adopt(spec, id, sys)
	slog.Info("VM found running", "vm", spec.Name)
}

// track starts watching a running system with the egress listener egressLn
// (nil for none), which it closes once the VM stops; m.mu is held.
func (m *Manager) track(spec Spec, id guid.GUID, sys brokerVM, egressLn net.Listener) {
	ctx, cancel := context.WithCancel(context.Background())
	j, ok := m.jobs[spec.ID]
	if !ok {
		j = newJobs()
		m.jobs[spec.ID] = j
	}
	r := &running{spec: spec, sys: sys, session: newSession(j, spec.OnReady), exited: make(chan struct{}), stopped: make(chan struct{}), cancel: cancel}
	r.session.onNotify = spec.OnNotify
	m.vms[spec.ID] = r
	go r.session.run(ctx, id)
	if egressLn != nil {
		go spec.Egress.Serve(egressLn)
	}
	go func() {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
	watch:
		for {
			// A broker restarting for an upgrade fails a call or two; the
			// VM runs on meanwhile.
			if state, err := sys.State(); (err == nil && state != broker.Running) || (err != nil && !brokerDown(err)) {
				break
			}
			select {
			case <-ctx.Done():
				break watch
			case <-t.C:
			}
		}
		close(r.exited)
		cancel()
		m.mu.Lock()
		delete(m.vms, spec.ID)
		m.mu.Unlock()
		// HCS calls can hang while the host shuts down; say which.
		slow(spec.Name, "terminating the compute system", func() { sys.Terminate() })
		if egressLn != nil {
			egressLn.Close()
		}
		j.stopped()
		close(r.stopped)
		slog.Info("VM stopped", "vm", spec.Name)
	}()
}

func (m *Manager) get(id string) (*running, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.vms[id]
	return r, ok
}

// Running reports whether the VM with ID id runs.
func (m *Manager) Running(id string) bool {
	_, ok := m.get(id)
	return ok
}

// Info is the VM's state for vm.status: "running" with its Guest Service, or
// "stopped".
func (m *Manager) Info(id string) (string, *api.GuestService) {
	r, ok := m.get(id)
	if !ok {
		return "stopped", nil
	}
	return "running", r.session.status()
}

// ExecRunning counts the VM's exec jobs that have not exited.
func (m *Manager) ExecRunning(id string) int {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j == nil {
		return 0
	}
	return j.running()
}

// ExecLost marks the VM's running exec jobs lost, once the Guest Service
// that ran them was replaced on the same boot and so will never report
// their exit.
func (m *Manager) ExecLost(id string) {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j != nil {
		j.stopped()
	}
}

// Stop powers the VM off: through the guest, waiting up to StopTimeout, then
// by turning it off. force turns it off at once, and ends a graceful Stop
// still waiting on the same VM. It returns once glovd has cleaned up after
// the VM, or cleanupGrace after HCS stopped running it.
func (m *Manager) Stop(ctx context.Context, id string, force bool) error {
	r, ok := m.get(id)
	if !ok {
		return ErrNotRunning
	}
	if !force {
		// HCS can leave the shutdown operation pending for good, as on a
		// guest whose integration services are not answering, so
		// StopTimeout bounds the call too. Terminate below does not wait
		// for it: both hold the handle's lock shared, and only Close, once
		// the VM has exited, takes it exclusively.
		shutdown := make(chan error, 1)
		go func() { shutdown <- r.sys.Shutdown() }()
		timeout := time.After(m.stopTimeout)
	wait:
		for {
			select {
			case err := <-shutdown:
				if err != nil {
					slog.Warn("graceful shutdown refused; turning the VM off", "vm", r.spec.Name, "err", err)
					break wait
				}
				shutdown = nil // taken; wait for the VM to exit
			case <-r.exited:
				return r.cleanedUp(ctx)
			case <-timeout:
				slog.Warn("the VM did not stop in time; turning it off", "vm", r.spec.Name, "timeout", m.stopTimeout)
				break wait
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	select {
	case <-r.exited:
	default:
		if err := r.sys.Terminate(); err != nil {
			slog.Warn("terminate", "vm", r.spec.Name, "err", err)
		}
	}
	r.cancel()
	select {
	case <-r.exited:
		return r.cleanedUp(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cleanupGrace bounds how long Stop waits, once the VM has exited, for
// glovd's cleanup after it; that waits on HCS, which can hang while
// the host shuts down.
const cleanupGrace = 10 * time.Second

func (r *running) cleanedUp(ctx context.Context) error {
	select {
	case <-r.stopped:
	case <-time.After(cleanupGrace):
		slog.Warn("the VM stopped but cleaning up after it has not finished", "vm", r.spec.Name, "after", cleanupGrace)
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// slow runs f, warning if it is still running after a few seconds.
func slow(vm, what string, f func()) {
	t := time.AfterFunc(5*time.Second, func() { slog.Warn("still "+what, "vm", vm) })
	defer t.Stop()
	f()
}

// Release revokes the base disk grant Start added for spec's VM, so a
// deleted VM leaves nothing behind on the shared base image, and gives
// its ID back to the broker.
func (m *Manager) Release(spec Spec) error {
	if spec.BaseDisk != "" {
		if err := hcs.RevokeVmAccess(spec.ID, spec.BaseDisk); err != nil {
			return err
		}
	}
	ctx, cancel := callContext()
	defer cancel()
	return m.host.ReleaseID(ctx, spec.ID)
}

// brokerDown reports an error from a broker that is not answering, as
// while it restarts.
func brokerDown(err error) bool {
	return errors.Is(err, broker.ErrNotInstalled) || errors.Is(err, rpc.ErrClosed) || errors.Is(err, context.DeadlineExceeded)
}

// WaitGuest waits until the VM's Guest Service answers.
func (m *Manager) WaitGuest(ctx context.Context, id string) error {
	for {
		r, ok := m.get(id)
		if !ok {
			return ErrNotRunning
		}
		if r.session.ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.stopped:
			return ErrNotRunning
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// logConsole copies the VM's serial console to path. HCS connects once per
// boot; a console left from before glovd restarted is not reattached.
func logConsole(ln net.Listener, path string) {
	defer ln.Close()
	c, err := ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	f, err := os.Create(path)
	if err != nil {
		slog.Warn("console log", "err", err)
		io.Copy(io.Discard, c)
		return
	}
	defer f.Close()
	// Truncating keeps the old creation time, which tells glov vm console
	// one boot's log from the next.
	now := windows.NsecToFiletime(time.Now().UnixNano())
	if err := windows.SetFileTime(windows.Handle(f.Fd()), &now, nil, nil); err != nil {
		slog.Warn("console log", "err", err)
	}
	io.Copy(f, c)
}
