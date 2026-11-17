package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/store"
)

// testOwner is the user tests serve, otherUser someone else.
const (
	testOwner = "S-1-5-21-1-2-3-1001"
	otherUser = "S-1-5-21-1-2-3-1002"
)

func pipePath(t *testing.T) string {
	return `\\.\pipe\glovd-test-` + strings.ReplaceAll(t.Name(), "/", "-")
}

// testCallers takes a control connection's caller from the line dialConn
// writes first.
type testCallers struct{}

func (testCallers) User(c net.Conn) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < 256 {
		if _, err := c.Read(b); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", errors.New("caller line too long")
}

// start serves a daemon for owner.
func start(t *testing.T, owner string, st store.Store) (addr, me string) {
	t.Helper()
	return serve(t, &Daemon{Owner: owner, Store: st, Version: "test", VMs: newFakeRunner()})
}

// serve serves d on a loopback port, with testCallers unless d has its own;
// me is the caller tests dial as.
func serve(t *testing.T, d *Daemon) (addr, me string) {
	t.Helper()
	if d.Callers == nil {
		d.Callers = testCallers{}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
	return ln.Addr().String(), testOwner
}

// dialConn connects to the daemon at addr as caller.
func dialConn(t *testing.T, addr, caller string) (net.Conn, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(c, caller+"\n"); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func dial(t *testing.T, addr, caller string) (*rpc.Client, error) {
	return dialNotify(t, addr, caller, nil)
}

func dialNotify(t *testing.T, addr, caller string, notify rpc.NotifyFunc) (*rpc.Client, error) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := dialConn(t, addr, caller)
	if err != nil {
		return nil, err
	}
	return handshake(ctx, c, notify)
}

// dialSSH opens an SSH relay to vm through the daemon at addr as caller.
func dialSSH(t *testing.T, ctx context.Context, addr, caller, vm string) (io.ReadWriteCloser, error) {
	c, err := dialConn(t, addr, caller)
	if err != nil {
		return nil, err
	}
	return sshRelay(ctx, c, vm)
}

func TestOwnerListsAndResolvesOnlyTheirVMs(t *testing.T) {
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	for _, r := range []store.Record{{Name: "dev", Owner: me}, {Name: "devbox", Owner: me}, {Name: "other", Owner: otherUser}} {
		if err := st.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	path, me := start(t, me, st)
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := t.Context()

	var vms []api.VMInfo
	if err := c.Call(ctx, api.MethodVMList, nil, &vms); err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 || vms[0].Name != "dev" || vms[1].Name != "devbox" || vms[0].State != "stopped" || vms[0].Engine != "hcs" {
		t.Fatalf("vm.list = %+v", vms)
	}
	var vm api.VMInfo
	if err := c.Call(ctx, api.MethodVMStatus, api.VMParams{VM: "devb"}, &vm); err != nil || vm.Name != "devbox" {
		t.Fatalf("vm.status devb = %+v, %v", vm, err)
	}
	err = c.Call(ctx, api.MethodVMStatus, api.VMParams{VM: "other"}, &vm)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeNotFound {
		t.Fatalf("vm.status of another user's VM err = %v, want not found", err)
	}
}

func TestCallerOtherThanOwnerIsRefused(t *testing.T) {
	path, me := start(t, otherUser, store.Store{Root: t.TempDir()})
	if c, err := dial(t, path, me); err == nil {
		c.Close()
		t.Fatal("a caller other than the owner got a connection")
	}
}

// creator serves a daemon whose image preparation and disk creation are fakes.
type creator struct {
	st       store.Store
	prepared chan struct{} // closed by Prepare, if set
	block    bool          // Prepare waits for its ctx to end
	diskErr  error
	provErr  error
	vm       provision.VM // what Provision was given
	distro   string       // what Prepare was given
	canceled chan error
}

func (cr *creator) daemon(owner string) *Daemon {
	return &Daemon{
		Owner: owner, Store: cr.st, Version: "test", VMs: newFakeRunner(), CADir: filepath.Join(cr.st.Root, "egress-ca"),
		Prepare: func(ctx context.Context, distro string, progress func(string)) (Image, error) {
			cr.distro = distro
			progress("downloading debian-test.qcow2")
			if cr.prepared != nil {
				close(cr.prepared)
			}
			if cr.block {
				<-ctx.Done()
				cr.canceled <- ctx.Err()
				return Image{}, ctx.Err()
			}
			return Image{Release: distro + "-test", BaseDisk: "C:/cache/" + distro + "-test/base.vhdx"}, nil
		},
		NewDisk: func(disk, base string, size uint64) error {
			if cr.diskErr != nil {
				return cr.diskErr
			}
			return os.WriteFile(disk, []byte(base+" "+strconv.FormatUint(size, 10)), 0o600)
		},
		Provision: func(ctx context.Context, vm provision.VM, progress func(string)) error {
			cr.vm = vm
			progress("installing the Guest Service")
			return cr.provErr
		},
	}
}

func TestCreateRecordsTheVMAndItsDisk(t *testing.T) {
	me := testOwner
	cr := &creator{st: store.Store{Root: t.TempDir()}}
	path, _ := serve(t, cr.daemon(me))
	var progress []string
	var mu sync.Mutex
	c, err := dialNotify(t, path, me, func(method string, params jsontext.Value) {
		var p api.Progress
		if method == api.NotifyProgress && json.Unmarshal(params, &p) == nil {
			mu.Lock()
			progress = append(progress, p.Message)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var vm api.VMInfo
	if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "dev", MemoryMiB: 2048}, &vm); err != nil {
		t.Fatal(err)
	}
	if vm.Name != "dev" || vm.State != "stopped" {
		t.Fatalf("vm.create = %+v", vm)
	}
	mu.Lock()
	if strings.Join(progress, "|") != "downloading debian-test.qcow2|provisioning: installing the Guest Service" {
		t.Errorf("progress = %q", progress)
	}
	mu.Unlock()
	r, err := cr.st.Load(me, "dev")
	if _, gerr := guid.FromString(r.ID); gerr != nil {
		t.Fatalf("record ID %q: %v", r.ID, gerr)
	}
	want := store.Record{Name: "dev", ID: r.ID, Provisioned: true, Owner: me, CPUs: min(api.DefaultCPUs, runtime.NumCPU()), MemoryMiB: 2048, DiskGiB: api.DefaultDiskGiB, Distro: "ubuntu", Release: "ubuntu-test", BaseDisk: "C:/cache/ubuntu-test/base.vhdx",
		Isolated: true, Egress: &store.Egress{Allow: []string{}}}
	if err != nil || !reflect.DeepEqual(r, want) {
		t.Fatalf("record = %+v, %v; want %+v", r, err, want)
	}
	if b, err := os.ReadFile(filepath.Join(cr.st.Dir("dev"), DiskFile)); err != nil || string(b) != "C:/cache/ubuntu-test/base.vhdx "+strconv.Itoa(api.DefaultDiskGiB<<30) {
		t.Fatalf("disk = %q, %v", b, err)
	}
	if v := cr.vm; v.ID != r.ID || v.Name != "dev" || v.Distro != "ubuntu" || v.Dir != cr.st.Dir("dev") || v.Disk != filepath.Join(v.Dir, DiskFile) || v.BaseDisk != r.BaseDisk || v.MemoryMiB != 2048 {
		t.Errorf("provisioned %+v", v)
	}
	err = c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "dev"}, &vm)
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("second create err = %v", err)
	}
	if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "deb", Distro: "debian"}, &vm); err != nil {
		t.Fatal(err)
	}
	if r, err := cr.st.Load(me, "deb"); err != nil || cr.distro != "debian" || r.Distro != "debian" || r.Release != "debian-test" || cr.vm.Distro != "debian" {
		t.Fatalf("debian: prepared %q, provisioned %+v, record %+v, %v", cr.distro, cr.vm, r, err)
	}
}

func TestCreateRefusesBadParamsBeforePreparing(t *testing.T) {
	me := testOwner
	cr := &creator{st: store.Store{Root: t.TempDir()}, prepared: make(chan struct{})}
	path, _ := serve(t, cr.daemon(me))
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, p := range []api.CreateParams{
		{VM: "Bad Name"},
		{VM: "dev", CPUs: runtime.NumCPU() + 1},
		{VM: "dev", CPUs: -1},
		{VM: "dev", MemoryMiB: 100},
		{VM: "dev", DiskGiB: api.MaxDiskGiB + 1},
		{VM: "dev", Distro: "arch"},
		{VM: "dev", Distro: "alpine"},
		{VM: "dev", Isolated: true, Project: &api.Project{Path: `p`, Sync: api.SyncGit}},
		{VM: "dev", Isolated: true, Project: &api.Project{Path: `C:\p`, Sync: "rsync"}},
	} {
		err := c.Call(t.Context(), api.MethodVMCreate, p, nil)
		if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams {
			t.Errorf("create %+v err = %v, want invalid params", p, err)
		}
	}
	select {
	case <-cr.prepared:
		t.Fatal("Prepare ran for bad params")
	default:
	}
}

func TestCreateWhoseDiskOrProvisioningFailsLeavesNoVM(t *testing.T) {
	me := testOwner
	for name, cr := range map[string]*creator{
		"disk":      {st: store.Store{Root: t.TempDir()}, diskErr: errors.New("no disk")},
		"provision": {st: store.Store{Root: t.TempDir()}, provErr: provision.ErrNoReport},
	} {
		t.Run(name, func(t *testing.T) {
			d := cr.daemon(me)
			path, _ := serve(t, d)
			c, err := dial(t, path, me)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "dev"}, nil); err == nil {
				t.Fatal("create succeeded")
			}
			if _, err := os.Stat(cr.st.Dir("dev")); !os.IsNotExist(err) {
				t.Fatalf("VM directory left behind: %v", err)
			}
			// Its ID goes back to the broker, its base disk grant away.
			if f := d.VMs.(*fakeRunner); strings.Join(f.released, ",") != "dev" {
				t.Fatalf("released %v, want dev", f.released)
			}
		})
	}
}

func TestCreateStopsWhenGlovGoesAway(t *testing.T) {
	me := testOwner
	cr := &creator{st: store.Store{Root: t.TempDir()}, prepared: make(chan struct{}), block: true, canceled: make(chan error, 1)}
	path, _ := serve(t, cr.daemon(me))
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	go c.Call(context.Background(), api.MethodVMCreate, api.CreateParams{VM: "dev"}, nil)
	<-cr.prepared
	c.Close()
	select {
	case err := <-cr.canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prepare ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prepare kept running after glov disconnected")
	}
	if _, err := os.Stat(cr.st.Dir("dev")); !os.IsNotExist(err) {
		t.Fatalf("VM directory created: %v", err)
	}
}

func TestQuitStopsGlovdOnRequest(t *testing.T) {
	me := testOwner
	quit := make(chan struct{})
	d := &Daemon{Owner: me, Store: store.Store{Root: t.TempDir()}, Version: "test", VMs: newFakeRunner(), Quit: func() { close(quit) }}
	path, _ := serve(t, d)
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(t.Context(), api.MethodQuit, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quit:
	case <-time.After(5 * time.Second):
		t.Fatal("Quit not called")
	}
}
