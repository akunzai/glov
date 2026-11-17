package broker

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/hcs"
	"github.com/akunzai/glov/windows/internal/rpc"
)

// fakeHCS keeps compute systems in memory.
type fakeHCS struct {
	mu      sync.Mutex
	systems map[string]*fakeSystem
}

type fakeSystem struct {
	h          *fakeHCS
	id         string
	vm         hcs.LinuxVM
	state      string
	terminated bool
	// ignoreShutdown makes the guest never take the shutdown request.
	ignoreShutdown bool
}

func (h *fakeHCS) Create(id string, vm hcs.LinuxVM) (System, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.systems == nil {
		h.systems = map[string]*fakeSystem{}
	}
	if _, ok := h.systems[id]; ok {
		return nil, errors.New("exists")
	}
	s := &fakeSystem{h: h, id: id, vm: vm, state: "Created"}
	h.systems[id] = s
	return s, nil
}

func (h *fakeHCS) Open(id string) (System, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.systems[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return s, nil
}

func (h *fakeHCS) state(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.systems[id]; ok {
		return s.state
	}
	return ""
}

func (s *fakeSystem) set(state string) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.state = state
}

func (s *fakeSystem) Start() error { s.set(Running); return nil }
func (s *fakeSystem) Shutdown() error {
	if !s.ignoreShutdown {
		s.set("Stopped")
	}
	return nil
}
func (s *fakeSystem) Terminate() error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.terminated = true
	delete(s.h.systems, s.id)
	return nil
}
func (s *fakeSystem) State() (string, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if _, ok := s.h.systems[s.id]; !ok {
		return "", errors.New("gone")
	}
	return s.state, nil
}
func (s *fakeSystem) Stats() (Stats, error) { return Stats{CPUTime100ns: 7}, nil }
func (s *fakeSystem) Close()                {}

func newServer(t *testing.T) (*Server, *fakeHCS) {
	t.Helper()
	h := &fakeHCS{}
	return &Server{Owners: &Owners{Path: filepath.Join(t.TempDir(), "owners.json")}, HCS: h, Version: "test", StopTimeout: 200 * time.Millisecond}, h
}

// connect serves one connection from sid in session, returning a client
// and a func that ends the connection and waits for the broker's cleanup.
func connect(t *testing.T, s *Server, sid string, session uint32) (*rpc.Client, func()) {
	t.Helper()
	return connectAs(t, s, Caller{SID: sid, Session: session, OpenDisk: func(string) (func(), error) { return func() {}, nil }})
}

func connectAs(t *testing.T, s *Server, caller Caller) (*rpc.Client, func()) {
	t.Helper()
	a, b := net.Pipe()
	methods, end := s.Methods(caller)
	done := make(chan struct{})
	go func() {
		methods.Serve(context.Background(), proto.NewConn(b))
		end()
		close(done)
	}()
	c := rpc.New(a, nil)
	var once sync.Once
	closeConn := func() {
		once.Do(func() {
			c.Close()
			<-done
		})
	}
	t.Cleanup(closeConn)
	return c, closeConn
}

func call(t *testing.T, c *rpc.Client, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Call(ctx, method, params, result)
}

func newID(t *testing.T, c *rpc.Client) string {
	t.Helper()
	var r IDParams
	if err := call(t, c, MethodNewID, nil, &r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func boot(t *testing.T, c *rpc.Client, id string, keep bool) {
	t.Helper()
	p := CreateParams{ID: id, CPUs: 1, MemoryMiB: 512, Disk: `C:\vms\a\disk.vhdx`, KeepRunning: keep}
	if err := call(t, c, MethodCreate, p, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := call(t, c, MethodStart, IDParams{ID: id}, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
}

const alice, bob = "S-1-5-21-1-1001", "S-1-5-21-1-1002"

func TestAnotherUserCannotDriveAVM(t *testing.T) {
	s, h := newServer(t)
	a, _ := connect(t, s, alice, 1)
	b, _ := connect(t, s, bob, 2)
	id := newID(t, a)
	boot(t, a, id, true)
	for _, m := range []string{MethodState, MethodShutdown, MethodTerminate, MethodStats, MethodReleaseID, MethodStart} {
		if err := call(t, b, m, IDParams{ID: id}, nil); err == nil {
			t.Errorf("%s by another user succeeded", m)
		}
	}
	if err := call(t, b, MethodCreate, CreateParams{ID: id, CPUs: 1, MemoryMiB: 512, Disk: `C:\x.vhdx`}, nil); err == nil {
		t.Error("create by another user succeeded")
	}
	if got := h.state(id); got != Running {
		t.Fatalf("state after another user's calls = %q, want Running", got)
	}
	if got := h.systems[id].vm; got.User != alice || got.Console != ConsolePipe(id) {
		t.Fatalf("document user %q console %q", got.User, got.Console)
	}
}

func TestAVMWithoutKeepRunningEndsWithItsConnection(t *testing.T) {
	s, h := newServer(t)
	a, closeA := connect(t, s, alice, 1)
	kept, provisioning := newID(t, a), newID(t, a)
	boot(t, a, kept, true)
	boot(t, a, provisioning, false)
	closeA()
	if got := h.state(provisioning); got != "" {
		t.Errorf("provisioning VM after its connection ended: %q, want gone", got)
	}
	if got := h.state(kept); got != Running {
		t.Errorf("kept VM after its connection ended: %q, want Running", got)
	}
	// glovd comes back and finds it.
	a2, _ := connect(t, s, alice, 1)
	var st State
	if err := call(t, a2, MethodState, IDParams{ID: kept}, &st); err != nil || st.State != Running {
		t.Fatalf("state = %+v, %v", st, err)
	}
	err := call(t, a2, MethodCreate, CreateParams{ID: kept, CPUs: 1, MemoryMiB: 512, Disk: `C:\x.vhdx`}, nil)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != CodeAlreadyRunning {
		t.Fatalf("create of a running VM = %v, want CodeAlreadyRunning", err)
	}
}

func TestACreatedVMNotStartedGoesWithItsConnection(t *testing.T) {
	s, h := newServer(t)
	a, closeA := connect(t, s, alice, 1)
	id := newID(t, a)
	if err := call(t, a, MethodCreate, CreateParams{ID: id, CPUs: 1, MemoryMiB: 512, Disk: `C:\x.vhdx`, KeepRunning: true}, nil); err != nil {
		t.Fatal(err)
	}
	closeA()
	if got := h.state(id); got != "" {
		t.Fatalf("state = %q, want gone", got)
	}
}

func TestReleaseRefusesARunningVMAndForgetsAStoppedOne(t *testing.T) {
	s, _ := newServer(t)
	a, _ := connect(t, s, alice, 1)
	id := newID(t, a)
	boot(t, a, id, true)
	if err := call(t, a, MethodReleaseID, IDParams{ID: id}, nil); err == nil {
		t.Fatal("released a running VM's ID")
	}
	if err := call(t, a, MethodTerminate, IDParams{ID: id}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(t, a, MethodReleaseID, IDParams{ID: id}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(t, a, MethodState, IDParams{ID: id}, nil); err == nil {
		t.Fatal("a released ID still answers")
	}
}

func TestSignOutStopsTheUsersVMsOnceTheirLastSessionEnds(t *testing.T) {
	s, h := newServer(t)
	a1, _ := connect(t, s, alice, 1)
	connect(t, s, alice, 3)
	b, _ := connect(t, s, bob, 2)
	ida, idb := newID(t, a1), newID(t, b)
	boot(t, a1, ida, true)
	boot(t, b, idb, true)
	h.systems[ida].ignoreShutdown = true // turned off after StopTimeout

	s.SignedOut(context.Background(), 1)
	if got := h.state(ida); got != Running {
		t.Fatalf("after one of two sessions signed out: %q, want Running", got)
	}
	s.SignedOut(context.Background(), 3)
	if got := h.state(ida); got != "" {
		t.Fatalf("after the last session signed out: %q, want gone", got)
	}
	if got := h.state(idb); got != Running {
		t.Fatalf("another user's VM: %q, want Running", got)
	}
}

func TestOwnersSurviveARestart(t *testing.T) {
	s, _ := newServer(t)
	a, _ := connect(t, s, alice, 1)
	id := newID(t, a)
	o := &Owners{Path: s.Owners.Path}
	if err := o.Check(id, alice); err != nil {
		t.Fatal(err)
	}
	if err := o.Check(id, bob); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Check for another user = %v", err)
	}
	if ids, _ := o.IDs(alice); len(ids) != 1 {
		t.Fatalf("IDs = %v", ids)
	}
}

func TestDiskPath(t *testing.T) {
	root := `C:\Users\a\AppData\Local\glov\vms`
	for path, ok := range map[string]bool{
		root + `\dev\disk.vhdx`:                           true,
		root + `\dev\seed.vhd`:                            true,
		`c:\users\A\appdata\local\glov\vms\dev\disk.VHDX`: true,
		root + `\dev\..\..\other.vhdx`:                    false,
		root + `\dev\disk.txt`:                            false,
		root + `.vhdx`:                                    false,
		`C:\Windows\disk.vhdx`:                            false,
		`\\server\share\glov\vms\disk.vhdx`:               false,
		`dev\disk.vhdx`:                                   false,
	} {
		if err := diskPath(root, path); (err == nil) != ok {
			t.Errorf("diskPath(%s) = %v, want ok %v", path, err, ok)
		}
	}
}

func TestCreatingAgainLetsGoOfTheFirstCreatesDisks(t *testing.T) {
	s, _ := newServer(t)
	var mu sync.Mutex
	pinned := 0
	a, closeA := connectAs(t, s, Caller{SID: alice, Session: 1, OpenDisk: func(string) (func(), error) {
		mu.Lock()
		defer mu.Unlock()
		pinned++
		return func() { mu.Lock(); pinned--; mu.Unlock() }, nil
	}})
	id := newID(t, a)
	for _, keep := range []bool{false, true, true} {
		if err := call(t, a, MethodCreate, CreateParams{ID: id, CPUs: 1, MemoryMiB: 512, Disk: `C:\x.vhdx`, KeepRunning: keep}, nil); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		if pinned != 1 {
			t.Errorf("after create (keep %v): %d disks pinned, want 1", keep, pinned)
		}
		mu.Unlock()
	}
	closeA()
	if pinned != 0 {
		t.Fatalf("after the connection ended: %d disks pinned", pinned)
	}
}

func TestAnIDIsSpelledOneWay(t *testing.T) {
	s, _ := newServer(t)
	a, _ := connect(t, s, alice, 1)
	id := newID(t, a)
	boot(t, a, id, true)
	if err := call(t, a, MethodState, IDParams{ID: strings.ToUpper(id)}, nil); err == nil {
		t.Fatal("an upper-case ID was taken")
	}
}

func TestSignOutKeepsTheVMsOfAUserSignedInElsewhere(t *testing.T) {
	s, h := newServer(t)
	s.SignedIn = func(sid string, except uint32) bool { return sid == alice && except == 1 }
	a, _ := connect(t, s, alice, 1)
	id := newID(t, a)
	boot(t, a, id, true)
	s.SignedOut(context.Background(), 1)
	if got := h.state(id); got != Running {
		t.Fatalf("state = %q, want Running: alice is still signed in to another session", got)
	}
}
