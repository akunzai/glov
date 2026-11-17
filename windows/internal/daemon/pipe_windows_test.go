package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// The tests here serve glovd on real named pipes, with the callers
// Windows reports: the rest of the package's tests use loopback ports and
// testCallers.

// servePipe serves d on a test pipe owned by the current user, who is me.
func servePipe(t *testing.T, d *Daemon) (path, me string) {
	t.Helper()
	me, err := winsec.CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	path = pipePath(t)
	ln, err := d.Listen(path, me)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Serve: %v", err)
		}
	})
	return path, me
}

func dialPipe(t *testing.T, path, owner string) (*rpc.Client, error) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return Dial(ctx, path, owner, nil)
}

func TestThePipeServesItsOwner(t *testing.T) {
	me, err := winsec.CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "dev", Owner: me}); err != nil {
		t.Fatal(err)
	}
	path, _ := servePipe(t, &Daemon{Owner: me, Store: st, Version: "test", VMs: newFakeRunner()})
	c, err := dialPipe(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var vms []api.VMInfo
	if err := c.Call(t.Context(), api.MethodVMList, nil, &vms); err != nil || len(vms) != 1 || vms[0].Name != "dev" {
		t.Errorf("vm.list = %+v, %v", vms, err)
	}
}

func TestThePipeRefusesACallerOtherThanTheOwner(t *testing.T) {
	// The test pipe's DACL admits this process as the server account, so the
	// connection opens and only glovd's own caller check refuses it.
	path, me := servePipe(t, &Daemon{Owner: otherUser, Store: store.Store{Root: t.TempDir()}, Version: "test", VMs: newFakeRunner()})
	if c, err := dialPipe(t, path, me); err == nil {
		c.Close()
		t.Fatal("a caller other than the owner got a connection")
	}
}

func TestPipeWithUnexpectedOwnerIsRefused(t *testing.T) {
	me, _ := winsec.CurrentSID()
	path, _ := servePipe(t, &Daemon{Owner: me, Store: store.Store{Root: t.TempDir()}, Version: "test", VMs: newFakeRunner()})
	_, err := dialPipe(t, path, winsec.SYSTEM)
	if err == nil || !strings.Contains(err.Error(), "another program holds glovd's pipe") {
		t.Fatalf("Dial with SYSTEM expected err = %v", err)
	}
}

func TestDialWithoutDaemonSaysItIsNotRunning(t *testing.T) {
	if _, err := dialPipe(t, pipePath(t), winsec.SYSTEM); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want ErrNotRunning", err)
	}
}
