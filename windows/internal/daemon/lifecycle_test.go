package daemon

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

// fakeRunner runs VMs in memory. A started VM's Guest Service is ready
// unless guestSilent is set.
type fakeRunner struct {
	mu       sync.Mutex
	running  map[string]bool
	started  []vmrun.Spec
	adopted  []string
	released []string
	// st and resized are lifecycle's store and ResizeDisk's calls.
	st          store.Store
	resized     map[string]uint64
	execs       []vmrun.ExecParams
	stops       []bool // force, per Stop
	guestSilent bool
	// hangGraceful, if set, makes a graceful Stop hang, as on a guest
	// that never takes the shutdown request: it closes the channel once
	// it waits, then returns only after a forced Stop has turned the VM off.
	hangGraceful chan struct{}
	forced       chan struct{}
	// pipe is the test daemon's control address.
	pipe string
	// guestSync answers the Guest Service's sync methods.
	guestSync func(ctx context.Context, method string, params, result any) error
	// execOut, if set, gives a job's output and exit code from its argv.
	execOut func(argv []string) (string, int)
	// conn is the Guest Service's connection ID ("1" when empty), jobs
	// the running exec jobs and staged what file.write wrote.
	conn   string
	jobs   int
	staged []byte
	// guestStats answers guest.stats; nil answers it as unknown.
	guestStats *api.GuestStats
	// secrets is each secrets.sync's params; secretsFail fails them.
	secrets     []any
	secretsFail error
}

func (f *fakeRunner) ExecRunning(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs
}

func (f *fakeRunner) ExecLost(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = 0
}

func (f *fakeRunner) Usage(id string) (vmrun.Usage, error) {
	if !f.Running(id) {
		return vmrun.Usage{}, vmrun.ErrNotRunning
	}
	n := uint64(1 << 30)
	return vmrun.Usage{CPUTime: time.Duration(time.Now().UnixNano()) / 2, Resident: &n, Committed: &n}, nil
}

func newFakeRunner() *fakeRunner { return &fakeRunner{running: map[string]bool{}} }

func (f *fakeRunner) NewID(context.Context) (string, error) {
	id, err := guid.NewV4()
	return id.String(), err
}

func (f *fakeRunner) Start(spec vmrun.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[spec.ID] = true
	f.started = append(f.started, spec)
	return nil
}

func (f *fakeRunner) Adopt(spec vmrun.Spec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopted = append(f.adopted, spec.Name)
}

func (f *fakeRunner) Stop(ctx context.Context, id string, force bool) error {
	if !force && f.hangGraceful != nil {
		close(f.hangGraceful)
		select {
		case <-f.forced:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if force && f.forced != nil {
		defer close(f.forced)
	}
	if !f.running[id] {
		return vmrun.ErrNotRunning
	}
	delete(f.running, id)
	f.stops = append(f.stops, force)
	return nil
}

func (f *fakeRunner) Release(spec vmrun.Spec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, spec.Name)
	return nil
}

func (f *fakeRunner) Running(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[id]
}

func (f *fakeRunner) Info(id string) (string, *api.GuestService) {
	if !f.Running(id) {
		return "stopped", nil
	}
	if f.guestSilent {
		return "running", &api.GuestService{State: "connecting"}
	}
	f.mu.Lock()
	conn := cmp.Or(f.conn, "1")
	f.mu.Unlock()
	return "running", &api.GuestService{State: "ready", ConnectionID: &conn}
}

func (f *fakeRunner) WaitGuest(ctx context.Context, id string) error {
	if !f.Running(id) {
		return vmrun.ErrNotRunning
	}
	if f.guestSilent {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

const devID = "6f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c"

// lifecycle serves a daemon with one provisioned VM, dev, and a VM that
// never provisioned, raw.
func lifecycle(t *testing.T, configure ...func(*Daemon)) (*fakeRunner, func(method string, params, result any) error) {
	t.Helper()
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	for _, r := range []store.Record{
		{Name: "dev", ID: devID, Owner: me, CPUs: 2, MemoryMiB: 1024, DiskGiB: 8, BaseDisk: `C:\cache\base.vhdx`, Provisioned: true, Isolated: true, Egress: &store.Egress{Allow: []string{}}},
		{Name: "raw", ID: "7f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c", Owner: me},
		{Name: "other", ID: "8f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c", Owner: otherUser, Provisioned: true},
	} {
		if err := st.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeRunner()
	f.st, f.resized = st, map[string]uint64{}
	resize := func(disk string, size uint64) error { f.resized[disk] = size; return nil }
	d := &Daemon{Owner: me, Store: st, Version: "test", VMs: f, ResizeDisk: resize, Callers: testCallers{},
		GuestBinary: func() ([]byte, error) { return []byte(newGuest), nil },
		HostMemory:  func() (api.HostMemory, error) { return api.HostMemory{Physical: 32 << 30}, nil }}
	for _, c := range configure {
		c(d)
	}
	path, _ := serve(t, d)
	f.pipe = path
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return f, func(method string, params, result any) error { return c.Call(t.Context(), method, params, result) }
}

func TestStartStatusStop(t *testing.T) {
	f, call := lifecycle(t)
	var vm api.VMInfo
	if err := call(api.MethodVMStart, api.VMParams{VM: "de"}, &vm); err != nil {
		t.Fatal(err)
	}
	if vm.State != "running" || vm.GuestService == nil {
		t.Fatalf("vm.start = %+v", vm)
	}
	if s := f.started[0]; s.ID != devID || s.Name != "dev" || !strings.HasSuffix(s.Disk, filepath.Join("vms", "dev", DiskFile)) || s.BaseDisk != `C:\cache\base.vhdx` || s.MemoryMiB != 1024 {
		t.Errorf("started %+v", s)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second start err = %v", err)
	}
	if err := call(api.MethodVMStatus, api.VMParams{VM: "dev"}, &vm); err != nil || vm.State != "running" || vm.GuestService.State != "ready" {
		t.Fatalf("vm.status = %+v, %v", vm, err)
	}
	if err := call(api.MethodVMStop, api.StopParams{VM: "dev", Force: true}, &vm); err != nil || vm.State != "stopped" {
		t.Fatalf("vm.stop = %+v, %v", vm, err)
	}
	if len(f.stops) != 1 || !f.stops[0] {
		t.Errorf("stops = %v, want one forced", f.stops)
	}
	if err := call(api.MethodVMStop, api.StopParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("stopping a stopped VM err = %v", err)
	}
}

func TestForcedStopIsNotHeldUpByAGracefulOne(t *testing.T) {
	f, call := lifecycle(t)
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	f.hangGraceful, f.forced = make(chan struct{}), make(chan struct{})
	graceful := make(chan error, 1)
	go func() { graceful <- call(api.MethodVMStop, api.StopParams{VM: "dev"}, nil) }()
	<-f.hangGraceful
	forced := make(chan error, 1)
	go func() { forced <- call(api.MethodVMStop, api.StopParams{VM: "dev", Force: true}, nil) }()
	for _, s := range []struct {
		name string
		done chan error
	}{{"forced", forced}, {"graceful", graceful}} {
		select {
		case err := <-s.done:
			if err != nil {
				t.Errorf("%s stop: %v", s.name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s stop still waiting", s.name)
		}
	}
}

func TestStartRefusesUnprovisionedAndOthersVMs(t *testing.T) {
	f, call := lifecycle(t, func(d *Daemon) {
		// A VM made before every Windows VM was isolated.
		if err := d.Store.Create(store.Record{Name: "wired", ID: "9f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c", Owner: testOwner, Provisioned: true}); err != nil {
			t.Fatal(err)
		}
	})
	if err := call(api.MethodVMStart, api.VMParams{VM: "raw"}, nil); err == nil || !strings.Contains(err.Error(), "never provisioned") {
		t.Errorf("start raw err = %v", err)
	}
	for _, method := range []string{api.MethodVMStart, api.MethodVMReboot} {
		if err := call(method, api.RebootParams{VM: "wired"}, nil); err == nil || !strings.Contains(err.Error(), "delete it and create it again") {
			t.Errorf("%s of a VM with a network adapter err = %v", method, err)
		}
	}
	err := call(api.MethodVMStart, api.VMParams{VM: "other"}, nil)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeNotFound {
		t.Errorf("start another user's VM err = %v, want not found", err)
	}
	if len(f.started) != 0 {
		t.Errorf("started %+v", f.started)
	}
}

func TestWait(t *testing.T) {
	f, call := lifecycle(t)
	err := call(api.MethodVMWait, api.WaitParams{VM: "dev", Timeout: 0.3}, nil)
	if err == nil || !strings.Contains(err.Error(), "not running after") {
		t.Errorf("wait running on a stopped VM err = %v", err)
	}
	err = call(api.MethodVMWait, api.WaitParams{VM: "dev", State: api.WaitGuestReady, Timeout: 0.3}, nil)
	if err == nil || !strings.Contains(err.Error(), "glov vm start dev") {
		t.Errorf("wait guest-ready on a stopped VM err = %v", err)
	}
	err = call(api.MethodVMWait, api.WaitParams{VM: "dev", State: "up"}, nil)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams {
		t.Errorf("wait with a bad state err = %v", err)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	var vm api.VMInfo
	if err := call(api.MethodVMWait, api.WaitParams{VM: "dev", State: api.WaitGuestReady}, &vm); err != nil || vm.GuestService.State != "ready" {
		t.Errorf("wait guest-ready = %+v, %v", vm, err)
	}
	f.guestSilent = true
	err = call(api.MethodVMWait, api.WaitParams{VM: "dev", State: api.WaitGuestReady, Timeout: 0.3}, nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 300ms") {
		t.Errorf("wait for a silent guest err = %v", err)
	}
}

func TestRebootStopsGracefullyAndStartsAgain(t *testing.T) {
	f, call := lifecycle(t)
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	var vm api.VMInfo
	if err := call(api.MethodVMReboot, api.RebootParams{VM: "dev"}, &vm); err != nil || vm.State != "running" {
		t.Fatalf("vm.reboot = %+v, %v", vm, err)
	}
	if len(f.started) != 2 || len(f.stops) != 1 || f.stops[0] {
		t.Errorf("started %d times, stops %v; want 2 and one graceful", len(f.started), f.stops)
	}
}

func TestAdoptLooksOnlyAtTheOwnersVMs(t *testing.T) {
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	for _, r := range []store.Record{{Name: "dev", ID: devID, Owner: me}, {Name: "other", ID: devID, Owner: otherUser}} {
		st.Create(r)
	}
	f := newFakeRunner()
	(&Daemon{Owner: me, Store: st, VMs: f}).Adopt()
	if strings.Join(f.adopted, ",") != "dev" {
		t.Errorf("adopted %v", f.adopted)
	}
}

func TestConfigure(t *testing.T) {
	f, call := lifecycle(t)
	me := testOwner
	var vm api.VMInfo
	if err := call(api.MethodVMConfigure, api.ConfigureParams{VM: "dev", CPUs: 1, DiskGiB: 12}, &vm); err != nil || vm.State != "stopped" {
		t.Fatalf("configure = %+v, %v", vm, err)
	}
	r, _ := f.st.Load(me, "dev")
	if r.CPUs != 1 || r.MemoryMiB != 1024 || r.DiskGiB != 12 || !r.Provisioned || r.ID != devID {
		t.Errorf("record after configure = %+v", r)
	}
	if len(f.resized) != 1 || f.resized[filepath.Join(f.st.Dir("dev"), DiskFile)] != 12<<30 {
		t.Errorf("resized = %v", f.resized)
	}
	clear(f.resized)
	if err := call(api.MethodVMConfigure, api.ConfigureParams{VM: "dev", MemoryMiB: 2048, DiskGiB: 12}, nil); err != nil || len(f.resized) != 0 {
		t.Errorf("configure without growing: err %v, resized %v", err, f.resized)
	}
	for _, p := range []api.ConfigureParams{
		{VM: "dev"},
		{VM: "dev", DiskGiB: 8},
		{VM: "dev", MemoryMiB: 100},
		{VM: "dev", CPUs: -1},
		{VM: "dev", DiskGiB: api.MaxDiskGiB + 1},
	} {
		err := call(api.MethodVMConfigure, p, nil)
		if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams {
			t.Errorf("configure %+v err = %v, want invalid params", p, err)
		}
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(api.MethodVMConfigure, api.ConfigureParams{VM: "dev", CPUs: 2}, nil); err == nil || !strings.Contains(err.Error(), "glov vm stop dev first") {
		t.Errorf("configure a running VM err = %v", err)
	}
}

func TestDelete(t *testing.T) {
	f, call := lifecycle(t)
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(api.MethodVMDelete, api.VMParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "is running") {
		t.Errorf("delete a running VM err = %v", err)
	}
	if err := call(api.MethodVMStop, api.StopParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"de", "other"} {
		err := call(api.MethodVMDelete, api.VMParams{VM: ref}, nil)
		if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeNotFound {
			t.Errorf("delete %s err = %v, want not found", ref, err)
		}
	}
	if err := call(api.MethodVMDelete, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.st.Dir("dev")); !os.IsNotExist(err) {
		t.Errorf("VM directory left: %v", err)
	}
	if _, err := os.Stat(f.st.Dir("other")); err != nil {
		t.Errorf("another user's VM is gone: %v", err)
	}
	if strings.Join(f.released, ",") != "dev" {
		t.Errorf("released %v", f.released)
	}
}

func (f *fakeRunner) ExecStart(_ context.Context, id string, p vmrun.ExecParams) (string, error) {
	if !f.Running(id) {
		return "", vmrun.ErrNotRunning
	}
	if p.As == "user" {
		return "", proto.Errorf(proto.CodeNoUser, "no user is signed in")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, p)
	return "job1", nil
}

func (f *fakeRunner) ExecRead(id, job string, cursor, max int) (vmrun.Read, error) {
	if job != "job1" {
		return vmrun.Read{}, vmrun.ErrNoJob
	}
	out := "hello\n"
	code := 3
	f.mu.Lock()
	if f.execOut != nil && len(f.execs) > 0 {
		out, code = f.execOut(f.execs[len(f.execs)-1].Argv)
	}
	f.mu.Unlock()
	data := out[min(cursor, len(out)):min(len(out), cursor+max)]
	return vmrun.Read{Data: []byte(data), Cursor: cursor + len(data), Exited: true, Code: &code}, nil
}

func (f *fakeRunner) ExecKill(_ context.Context, id, job string) error {
	if job != "job1" {
		return vmrun.ErrNoJob
	}
	return nil
}

func (f *fakeRunner) GuestCall(ctx context.Context, id, method string, params, result any) error {
	if !f.Running(id) {
		return vmrun.ErrNotRunning
	}
	if f.guestSync != nil && strings.HasPrefix(method, "sync.") {
		return f.guestSync(ctx, method, params, result)
	}
	switch method {
	case "file.write":
		p := params.(map[string]any)
		f.mu.Lock()
		if p["truncate"] == true {
			f.staged = nil
		}
		f.staged = append(f.staged, p["data"].([]byte)...)
		f.mu.Unlock()
		return nil
	case "secrets.sync":
		f.mu.Lock()
		defer f.mu.Unlock()
		f.secrets = append(f.secrets, params)
		return f.secretsFail
	case "guest.stats":
		if f.guestStats == nil {
			return proto.Errorf(proto.CodeNoMethod, "unknown method")
		}
		*result.(*api.GuestStats) = *f.guestStats
		return nil
	}
	*result.(*jsontext.Value) = jsontext.Value(`{"method":"` + method + `","params":` + string(params.(jsontext.Value)) + `}`)
	return nil
}

func TestExec(t *testing.T) {
	f, call := lifecycle(t)
	err := call(api.MethodExecStart, api.ExecParams{VM: "dev", Argv: []string{"true"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "glov vm start dev") {
		t.Errorf("exec on a stopped VM err = %v", err)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []api.ExecParams{{VM: "dev"}, {VM: "dev", Argv: []string{""}}, {VM: "dev", Argv: []string{"id"}, As: "root"}} {
		err := call(api.MethodExecStart, p, nil)
		if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams {
			t.Errorf("exec %+v err = %v, want invalid params", p, err)
		}
	}
	err = call(api.MethodExecStart, api.ExecParams{VM: "dev", Argv: []string{"id"}, As: "user"}, nil)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeNoUser {
		t.Errorf("exec as a missing user err = %v, want code %d", err, proto.CodeNoUser)
	}
	var job api.JobStarted
	p := api.ExecParams{VM: "de", Argv: []string{"/bin/sh", "-c", "echo hello"}, Cwd: "/tmp", Env: map[string]string{"A": "1"}}
	if err := call(api.MethodExecStart, p, &job); err != nil || job.JobID != "job1" {
		t.Fatalf("exec.start = %+v, %v", job, err)
	}
	if e := f.execs[0]; e.Cwd != "/tmp" || e.Env["A"] != "1" || len(e.Argv) != 3 {
		t.Errorf("guest got %+v", e)
	}
	var r api.ExecRead
	if err := call(api.MethodExecRead, api.ExecReadParams{VM: "dev", JobID: "job1", Cursor: 2}, &r); err != nil || string(r.Data) != "llo\n" || r.Cursor != 6 || !r.Exited || *r.Code != 3 {
		t.Fatalf("exec.read = %+v, %v", r, err)
	}
	err = call(api.MethodExecRead, api.ExecReadParams{VM: "dev", JobID: "nope"}, nil)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeNotFound {
		t.Errorf("read an unknown job err = %v, want not found", err)
	}
	if err := call(api.MethodExecKill, api.ExecKillParams{VM: "dev", JobID: "job1"}, nil); err != nil {
		t.Errorf("exec.kill: %v", err)
	}
}

func TestGuestCallPassesOnlyFileMethods(t *testing.T) {
	_, call := lifecycle(t)
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Method string `json:"method"`
		Params struct {
			Path string `json:"path"`
		} `json:"params"`
	}
	if err := call(api.MethodGuestCall, api.GuestCallParams{VM: "dev", Method: "file.stat", Params: jsontext.Value(`{"path":"/etc"}`)}, &res); err != nil || res.Method != "file.stat" || res.Params.Path != "/etc" {
		t.Fatalf("guest.call = %+v, %v", res, err)
	}
	for _, m := range []string{"exec.start", "power.reboot", "sync.apply", "guest.info"} {
		err := call(api.MethodGuestCall, api.GuestCallParams{VM: "dev", Method: m, Params: jsontext.Value(`{}`)}, nil)
		if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams {
			t.Errorf("guest.call %s err = %v, want invalid params", m, err)
		}
	}
}

// DialSSH answers with an echo server standing in for sshd.
func (f *fakeRunner) DialSSH(_ context.Context, id string) (io.ReadWriteCloser, error) {
	if !f.Running(id) {
		return nil, vmrun.ErrNotRunning
	}
	a, b := net.Pipe()
	go func() { io.Copy(b, b); b.Close() }()
	return a, nil
}

func TestSSHRelay(t *testing.T) {
	f, call := lifecycle(t)
	me := testOwner
	ctx := t.Context()
	if _, err := dialSSH(t, ctx, f.pipe, me, "dev"); err == nil || !strings.Contains(err.Error(), "not running: glov vm start dev") {
		t.Errorf("relay to a stopped VM err = %v", err)
	}
	if _, err := dialSSH(t, ctx, f.pipe, me, "other"); err == nil || strings.Contains(err.Error(), "rpc error") {
		t.Errorf("relay to another user's VM err = %v", err)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	c, err := dialSSH(t, ctx, f.pipe, me, "de")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "SSH-2.0-test\r\n"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 14)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "SSH-2.0-test\r\n" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	// The control pipe still speaks JSON-RPC alongside.
	if err := call(api.MethodVMStatus, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSSHRelayRefusesBadHeaders(t *testing.T) {
	for _, header := range []string{"glov-ssh \n", "glov-sshdev\n", "g" + strings.Repeat("x", api.SSHHeaderMax)} {
		r := bufio.NewReader(strings.NewReader(header))
		if vm, err := readHeader(r); err == nil {
			t.Errorf("readHeader(%.20q) = %q", header, vm)
		}
	}
	vm, err := readHeader(bufio.NewReader(strings.NewReader("glov-ssh dev\nrest")))
	if err != nil || vm != "dev" {
		t.Errorf("readHeader = %q, %v", vm, err)
	}
}

func TestWaitReportsProgress(t *testing.T) {
	f, call := lifecycle(t)
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	f.guestSilent = true
	var lines []string
	var mu sync.Mutex
	c, err := dialNotify(t, f.pipe, testOwner, func(method string, params jsontext.Value) {
		var p api.Progress
		if method == api.NotifyProgress && json.Unmarshal(params, &p) == nil {
			mu.Lock()
			lines = append(lines, p.Message)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(t.Context(), api.MethodVMWait, api.WaitParams{VM: "dev", State: api.WaitGuestReady, Timeout: 1.5}, nil); err == nil {
		t.Fatal("wait for a silent guest succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 || lines[0] != "waiting for dev to be guest-ready (up to 1s)" || !strings.HasPrefix(lines[1], "dev: running, Guest Service ") {
		t.Errorf("progress = %q, want what it waits for, then the VM's state", lines)
	}
}
