package vmrun

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/egress"
)

// hungSystem is a compute system whose guest never takes the shutdown
// request: Shutdown blocks until the test ends. Terminate turns it off and
// does what glovd's watch on the VM does once it is off.
type hungSystem struct {
	release    chan struct{}
	r          *running
	off        sync.Once
	terminated chan struct{}
}

func (s *hungSystem) Shutdown() error {
	<-s.release
	return nil
}

func (s *hungSystem) Terminate() error {
	s.off.Do(func() {
		close(s.terminated)
		close(s.r.exited)
		close(s.r.stopped)
	})
	return nil
}

func (s *hungSystem) Stats() (broker.Stats, error) { return broker.Stats{}, nil }

// hungVM tracks a VM on a hungSystem in m.
func hungVM(t *testing.T, m *Manager) *hungSystem {
	t.Helper()
	s := &hungSystem{release: make(chan struct{}), terminated: make(chan struct{})}
	t.Cleanup(func() { close(s.release) })
	s.r = &running{spec: Spec{ID: "vm1", Name: "dev"}, sys: s, exited: make(chan struct{}), stopped: make(chan struct{}), cancel: func() {}}
	m.vms["vm1"] = s.r
	return s
}

// stopWithin runs m.Stop and fails the test unless it returns nil within d.
func stopWithin(t *testing.T, m *Manager, force bool, d time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- m.Stop(t.Context(), "vm1", force) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop(force=%v) = %v", force, err)
		}
	case <-time.After(d):
		t.Fatalf("Stop(force=%v) still waiting after %s", force, d)
	}
}

func TestStopTurnsOffAVMWhoseShutdownHangs(t *testing.T) {
	m := NewManager(nil)
	m.stopTimeout = 100 * time.Millisecond
	s := hungVM(t, m)
	stopWithin(t, m, false, 5*time.Second)
	select {
	case <-s.terminated:
	default:
		t.Fatal("the VM was not turned off")
	}
}

func TestForcedStopEndsAPendingGracefulOne(t *testing.T) {
	m := NewManager(nil)
	m.stopTimeout = time.Hour
	s := hungVM(t, m)
	graceful := make(chan error, 1)
	go func() { graceful <- m.Stop(context.Background(), "vm1", false) }()
	stopWithin(t, m, true, 5*time.Second)
	select {
	case err := <-graceful:
		if err != nil {
			t.Fatalf("graceful Stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("graceful Stop still waiting after the VM was turned off")
	}
	select {
	case <-s.terminated:
	default:
		t.Fatal("the VM was not turned off")
	}
}

// consoleHost is a broker whose Create connects to the VM's console pipe
// as the VM's worker does, once, and writes a line; the rest refuses.
type consoleHost struct {
	Host
	dialed chan error
}

func (h *consoleHost) Create(ctx context.Context, p broker.CreateParams) error {
	timeout := 500 * time.Millisecond
	c, err := winio.DialPipe(broker.ConsolePipe(p.ID), &timeout)
	if err == nil {
		_, err = c.Write([]byte("UEFI firmware\n"))
		c.Close()
	}
	h.dialed <- err
	return nil
}

func (h *consoleHost) Terminate(ctx context.Context, id string) error { return nil }

// The worker connects to COM1 as the system is created; a console pipe
// that started accepting only after Create lost every boot's log.
func TestConsoleAcceptsTheWorkerDuringCreate(t *testing.T) {
	g, err := guid.NewV4()
	if err != nil {
		t.Fatal(err)
	}
	h := &consoleHost{dialed: make(chan error, 1)}
	m := NewManager(h)
	dir := t.TempDir()
	// The egress listener fails for a system that does not exist, so
	// Start stops after Create; the console was connected by then.
	m.Start(Spec{ID: g.String(), Name: "dev", Dir: dir, Egress: &egress.Proxy{}})
	if err := <-h.dialed; err != nil {
		t.Fatalf("the worker could not connect to the console: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, ConsoleLog))
		if string(b) == "UEFI firmware\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console.log = %q", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
