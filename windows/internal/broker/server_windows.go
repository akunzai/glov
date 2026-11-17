package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/hcs"
)

// System is a compute system the broker holds open; *hcs.System through
// HCSSystems is the real one.
type System interface {
	Start() error
	Shutdown() error
	Terminate() error
	State() (string, error)
	Stats() (Stats, error)
	Close()
}

// Systems creates and opens compute systems.
type Systems interface {
	Create(id string, vm hcs.LinuxVM) (System, error)
	// Open opens the system with ID id; it fails if HCS has none.
	Open(id string) (System, error)
}

// Caller is who is on a connection.
type Caller struct {
	SID string
	// Session is the caller's Remote Desktop session (TokenSessionId), the
	// one WTS_SESSION_LOGOFF names at sign-out; not a logon session LUID.
	Session uint32
	// OpenDisk checks that path is a disk the caller may boot, opening it
	// as the caller, and keeps it and its folders from being renamed or
	// replaced until release runs.
	OpenDisk func(path string) (release func(), err error)
}

// DefaultStopTimeout bounds a guest's own shutdown when the broker stops
// VMs (sign-out, host shutdown), as glovd's vm stop does.
const DefaultStopTimeout = 60 * time.Second

// Server serves the broker's methods.
type Server struct {
	Owners  *Owners
	HCS     Systems
	Version string
	// StopTimeout is DefaultStopTimeout, shortened by tests.
	StopTimeout time.Duration

	mu       sync.Mutex
	held     map[string]System // by ID: VMs started to keep running, or found running
	sessions map[uint32]string // Remote Desktop session -> SID of a user who called from it
	// SignedIn reports whether sid is signed in to a session other than
	// except; nil counts only the sessions callers came from.
	SignedIn func(sid string, except uint32) bool
}

// Methods returns the handlers for one connection from c. The returned
// end runs when the connection ends: it turns off the VMs started on it
// without KeepRunning.
func (s *Server) Methods(c Caller) (methods *proto.Server, end func()) {
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[uint32]string{}
	}
	s.sessions[c.Session] = c.SID
	s.mu.Unlock()

	conn := newConnVMs()
	m := proto.NewServer()
	m.Handle(MethodHello, func(Hello) (Hello, error) {
		return Hello{Protocol: Protocol, Version: s.Version}, nil
	})
	m.Handle(MethodNewID, func(struct{}) (IDParams, error) {
		// Any signed-in user may call; keep one from growing the file
		// without end.
		if ids, err := s.Owners.IDs(c.SID); err != nil {
			return IDParams{}, err
		} else if len(ids) >= MaxIDs {
			return IDParams{}, proto.Errorf(proto.CodeInvalidParams, "you hold %d VM IDs already; delete VMs first", len(ids))
		}
		id, err := guid.NewV4()
		if err != nil {
			return IDParams{}, err
		}
		if err := s.Owners.Add(id.String(), c.SID); err != nil {
			return IDParams{}, err
		}
		slog.Info("VM ID handed out", "id", id.String(), "user", c.SID)
		return IDParams{ID: id.String()}, nil
	})
	m.HandleAsync(MethodReleaseID, func(p IDParams) (struct{}, error) {
		if err := s.own(c, p.ID); err != nil {
			return struct{}{}, err
		}
		if sys := s.system(conn, p.ID); sys != nil {
			if state, _ := sys.State(); state == Running {
				return struct{}{}, proto.Errorf(proto.CodeInvalidParams, "VM %s is running", p.ID)
			}
			s.forget(conn, p.ID, sys)
		}
		return struct{}{}, s.Owners.Remove(p.ID)
	})
	m.HandleAsync(MethodCreate, func(p CreateParams) (struct{}, error) {
		return struct{}{}, s.create(conn, c, p)
	})
	m.HandleAsync(MethodStart, func(p IDParams) (struct{}, error) {
		return struct{}{}, s.start(conn, c, p.ID)
	})
	m.Handle(MethodState, func(p IDParams) (State, error) {
		if err := s.own(c, p.ID); err != nil {
			return State{}, err
		}
		sys := s.system(conn, p.ID)
		if sys == nil {
			return State{}, nil
		}
		state, err := sys.State()
		if err != nil {
			// HCS has cleaned the stopped system up already.
			return State{}, nil
		}
		return State{State: state}, nil
	})
	m.HandleAsync(MethodShutdown, func(p IDParams) (struct{}, error) {
		sys, err := s.runningSystem(conn, c, p.ID)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, sys.Shutdown()
	})
	m.HandleAsync(MethodTerminate, func(p IDParams) (struct{}, error) {
		if err := s.own(c, p.ID); err != nil {
			return struct{}{}, err
		}
		if sys := s.system(conn, p.ID); sys != nil {
			s.forget(conn, p.ID, sys)
		}
		return struct{}{}, nil
	})
	m.Handle(MethodStats, func(p IDParams) (Stats, error) {
		sys, err := s.runningSystem(conn, c, p.ID)
		if err != nil {
			return Stats{}, err
		}
		return sys.Stats()
	})
	return m, func() {
		conn.mu.Lock()
		vms, pending := conn.vms, conn.created
		conn.vms, conn.created = map[string]System{}, map[string]created{}
		conn.mu.Unlock()
		for id, cr := range pending {
			cr.release()
			if _, ok := vms[id]; !ok {
				slog.Info("removing a VM created but not started", "id", id, "user", c.SID)
				s.forget(conn, id, cr.sys)
			}
		}
		for id, sys := range vms {
			slog.Info("turning off a VM whose connection ended", "id", id, "user", c.SID)
			sys.Terminate()
			sys.Close()
		}
	}
}

// connVMs are one connection's VMs: those created on it without
// KeepRunning, and those created on it and not started yet.
type connVMs struct {
	mu      sync.Mutex
	vms     map[string]System
	created map[string]created
}

// created is a VM created and not started yet, with its disks pinned.
type created struct {
	sys     System
	release func()
}

func newConnVMs() *connVMs {
	return &connVMs{vms: map[string]System{}, created: map[string]created{}}
}

func (s *Server) own(c Caller, id string) error {
	// One spelling per ID, as id.new hands it out: the broker's maps are
	// keyed by it.
	if g, err := guid.FromString(id); err != nil || g.String() != id {
		return proto.Errorf(proto.CodeInvalidParams, "VM ID %q: not as id.new spells it", id)
	}
	if err := s.Owners.Check(id, c.SID); err != nil {
		if errors.Is(err, ErrNotOwner) {
			return proto.Errorf(proto.CodeInvalidParams, "VM %s: %v", id, err)
		}
		return err
	}
	return nil
}

// system returns the system with ID id: one started on conn, one the
// broker holds, or else one HCS has, which the broker then holds. It
// returns nil if HCS has none.
func (s *Server) system(conn *connVMs, id string) System {
	conn.mu.Lock()
	sys, ok := conn.vms[id]
	conn.mu.Unlock()
	if ok {
		return sys
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sys, ok := s.held[id]; ok {
		return sys
	}
	sys, err := s.HCS.Open(id)
	if err != nil {
		return nil
	}
	s.hold(id, sys)
	return sys
}

// hold keeps sys as the system with ID id; s.mu is held.
func (s *Server) hold(id string, sys System) {
	if s.held == nil {
		s.held = map[string]System{}
	}
	s.held[id] = sys
}

// forget turns sys off and closes it, and drops it from conn and the held
// systems.
func (s *Server) forget(conn *connVMs, id string, sys System) {
	sys.Terminate()
	conn.mu.Lock()
	if conn.vms[id] == sys {
		delete(conn.vms, id)
	}
	cr, pending := conn.created[id]
	if pending && cr.sys == sys {
		delete(conn.created, id)
	}
	conn.mu.Unlock()
	if pending && cr.sys == sys {
		cr.release() // the pins of a VM created but never started
	}
	s.mu.Lock()
	if s.held[id] == sys {
		delete(s.held, id)
	}
	s.mu.Unlock()
	sys.Close()
}

func (s *Server) runningSystem(conn *connVMs, c Caller, id string) (System, error) {
	if err := s.own(c, id); err != nil {
		return nil, err
	}
	sys := s.system(conn, id)
	if sys == nil {
		return nil, proto.Errorf(proto.CodeInvalidParams, "VM %s is not running", id)
	}
	return sys, nil
}

func (s *Server) create(conn *connVMs, c Caller, p CreateParams) error {
	if err := s.own(c, p.ID); err != nil {
		return err
	}
	if _, err := guid.FromString(p.ID); err != nil {
		return proto.Errorf(proto.CodeInvalidParams, "VM ID %q: %v", p.ID, err)
	}
	switch {
	case p.CPUs < 1 || p.CPUs > runtime.NumCPU():
		return proto.Errorf(proto.CodeInvalidParams, "cpus must be 1 to %d", runtime.NumCPU())
	case p.MemoryMiB < 256 || p.MemoryMiB > 1<<20:
		return proto.Errorf(proto.CodeInvalidParams, "memory must be 256 MiB to 1 TiB")
	case p.Disk == "":
		return proto.Errorf(proto.CodeInvalidParams, "no disk")
	}
	// A system left from an earlier run holds the ID: a running one is the
	// caller's to adopt, a stopped one goes.
	if sys := s.system(conn, p.ID); sys != nil {
		if state, _ := sys.State(); state == Running {
			return &proto.Error{Code: CodeAlreadyRunning, Message: fmt.Sprintf("VM %s is already running", p.ID)}
		}
		s.forget(conn, p.ID, sys)
	}
	var releases []func()
	release := func() {
		for _, r := range releases {
			r()
		}
	}
	for _, disk := range []string{p.Disk, p.Seed} {
		if disk == "" {
			continue
		}
		r, err := c.OpenDisk(disk)
		if err != nil {
			release()
			return proto.Errorf(proto.CodeInvalidParams, "%v", err)
		}
		releases = append(releases, r)
	}
	sys, err := s.HCS.Create(p.ID, hcs.LinuxVM{
		Owner: OwnerPrefix + c.SID, User: c.SID, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB,
		Disk: p.Disk, Seed: p.Seed, Console: ConsolePipe(p.ID), KeepRunning: p.KeepRunning, SecureBoot: p.SecureBoot,
	})
	if err != nil {
		release()
		return fmt.Errorf("create the VM: %w", err)
	}
	conn.mu.Lock()
	conn.created[p.ID] = created{sys: sys, release: release}
	if !p.KeepRunning {
		conn.vms[p.ID] = sys
	}
	conn.mu.Unlock()
	if p.KeepRunning {
		s.mu.Lock()
		s.hold(p.ID, sys)
		s.mu.Unlock()
	}
	return nil
}

func (s *Server) start(conn *connVMs, c Caller, id string) error {
	if err := s.own(c, id); err != nil {
		return err
	}
	conn.mu.Lock()
	cr, ok := conn.created[id]
	delete(conn.created, id)
	conn.mu.Unlock()
	if !ok {
		return proto.Errorf(proto.CodeInvalidParams, "VM %s was not created on this connection", id)
	}
	// HCS opens the disks now; then they need no pinning.
	err := cr.sys.Start()
	cr.release()
	if err != nil {
		s.forget(conn, id, cr.sys)
		return fmt.Errorf("start the VM: %w", err)
	}
	slog.Info("VM started", "id", id, "user", c.SID)
	return nil
}

// SignedOut stops the VMs of the user who called from session, unless
// they are still signed in to another session: glov there starts glovd
// again.
func (s *Server) SignedOut(ctx context.Context, session uint32) {
	s.mu.Lock()
	sid, ok := s.sessions[session]
	delete(s.sessions, session)
	for _, other := range s.sessions {
		if other == sid {
			ok = false
		}
	}
	s.mu.Unlock()
	if !ok || (s.SignedIn != nil && s.SignedIn(sid, session)) {
		return
	}
	slog.Info("user signed out; stopping their VMs", "user", sid, "session", session)
	s.Stop(ctx, sid)
}

// Stop shuts down the running VMs handed to sid, or every user's for sid
// "", all at once: through the guest, then by turning each off after
// StopTimeout. It returns when they have stopped or ctx ends.
func (s *Server) Stop(ctx context.Context, sid string) {
	ids, err := s.Owners.IDs(sid)
	if err != nil {
		slog.Warn("listing VM IDs", "err", err)
		return
	}
	timeout := s.StopTimeout
	if timeout == 0 {
		timeout = DefaultStopTimeout
	}
	none := newConnVMs()
	var wg sync.WaitGroup
	for _, id := range ids {
		sys := s.system(none, id)
		if sys == nil {
			continue
		}
		if state, _ := sys.State(); state != Running {
			continue
		}
		wg.Go(func() {
			stopOne(ctx, sys, timeout)
			s.forget(none, id, sys)
			slog.Info("VM stopped", "id", id)
		})
	}
	wg.Wait()
}

// stopOne asks sys's guest to power off and waits up to timeout for it.
func stopOne(ctx context.Context, sys System, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// HCS can leave the shutdown operation pending for good; forget then
	// terminates the system, which ends it.
	go sys.Shutdown()
	for {
		if state, err := sys.State(); err != nil || state != Running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
