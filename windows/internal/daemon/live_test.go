package daemon

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/share"
)

type notice struct {
	method string
	p      api.ShareChanged
}

// liveAgent dials as a glov share agent and returns what glovd tells it.
func liveAgent(t *testing.T, f *fakeRunner) (func(method string, params, result any) error, <-chan notice, func()) {
	t.Helper()
	got := make(chan notice, 16)
	c, err := dialNotify(t, f.pipe, testOwner, func(method string, params jsontext.Value) {
		var p api.ShareChanged
		json.Unmarshal(params, &p)
		got <- notice{method, p}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return func(method string, params, result any) error { return c.Call(t.Context(), method, params, result) }, got, func() { c.Close() }
}

func next(t *testing.T, got <-chan notice) notice {
	t.Helper()
	select {
	case n := <-got:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
		return notice{}
	}
}

func TestALiveShareIsHeldByItsAgentWhichGlovdTellsOfChanges(t *testing.T) {
	var d *Daemon
	f, call := lifecycle(t, func(dd *Daemon) { d = dd })
	g := &fakeGuestSync{}
	f.guestSync = g.answer
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range []share.Share{{Name: "proj", HostPath: `C:\src\Proj`}, {Name: "live", HostPath: `C:\src\Live`, Mode: share.Live}} {
		if err := call(api.MethodShareAdd, api.ShareAddParams{VM: "dev", Share: s}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "live"}, nil); err == nil || !strings.Contains(err.Error(), "is live") {
		t.Errorf("a mirror sync of a live share = %v", err)
	}
	agent, got, closeAgent := liveAgent(t, f)
	if err := agent(api.MethodShareLiveBegin, api.ShareParams{VM: "dev", Name: "proj"}, nil); err == nil || !strings.Contains(err.Error(), "is not live") {
		t.Errorf("liveBegin of a mirror share = %v", err)
	}
	var begun api.ShareSyncBegun
	if err := agent(api.MethodShareLiveBegin, api.ShareParams{VM: "de", Name: "live"}, &begun); err != nil {
		t.Fatal(err)
	}
	if begun.VM != "dev" || begun.Share.Mode != share.Live {
		t.Errorf("begun = %+v", begun)
	}
	if err := call(api.MethodShareLiveBegin, api.ShareParams{VM: "dev", Name: "live"}, nil); err == nil || !strings.Contains(err.Error(), "another glov share agent") {
		t.Errorf("a second agent = %v", err)
	}
	if err := call(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "live"}, nil); err == nil {
		t.Error("a sync began on a held live share")
	}

	watch := api.ShareSyncCallParams{VM: "dev", Name: "live", Method: "sync.watch", Params: jsontext.Value(`{"exclude":[]}`)}
	if err := call(api.MethodShareSyncCall, watch, nil); err == nil {
		t.Error("another connection's sync.watch was passed on")
	}
	if err := agent(api.MethodShareSyncCall, watch, nil); err != nil {
		t.Fatal(err)
	}
	if w := g.called("sync.watch"); len(w) != 1 || w[0] != `sync.watch {"exclude":[],"id":"live","root":"/mnt/c/src/Live"}` {
		t.Errorf("guest calls = %q", w)
	}

	d.guestNotify("dev", "sync.changed", jsontext.Value(`{"id":"proj","paths":["x"]}`)) // not held: dropped
	d.guestNotify("dev", "sync.changed", jsontext.Value(`{"id":"live","paths":["a/b.txt"]}`))
	if n := next(t, got); n.method != api.NotifyShareChanged || n.p.Name != "live" || len(n.p.Paths) != 1 || n.p.Paths[0] != "a/b.txt" {
		t.Errorf("changed = %+v", n)
	}
	d.liveReady("dev")
	if n := next(t, got); n.method != api.NotifyShareRewatch || n.p.Name != "live" {
		t.Errorf("rewatch = %+v", n)
	}
	if err := call(api.MethodShareLiveReconcile, api.ShareParams{VM: "dev", Name: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if n := next(t, got); n.method != api.NotifyShareReconcile {
		t.Errorf("reconcile = %+v", n)
	}

	cf := api.ShareLiveConflictParams{VM: "dev", Name: "live", Path: "a/b.txt", Winner: "guest", Why: "newer"}
	if err := call(api.MethodShareLiveConflict, cf, nil); err == nil {
		t.Error("a conflict from another connection was kept")
	}
	if err := agent(api.MethodShareLiveConflict, cf, nil); err != nil {
		t.Fatal(err)
	}
	var status []api.SyncProgress
	if err := call(api.MethodShareStatus, api.ShareStatusParams{VM: "dev"}, &status); err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status[0].Stage != api.StageLive || len(status[0].Conflicts) != 1 || status[0].Conflicts[0].Path != "a/b.txt" {
		t.Errorf("share.status = %+v", status)
	}

	// A reset the agent missed: its base is of the disk before.
	r, _ := f.st.Load(testOwner, "dev")
	r.Disk++
	f.st.Save(r)
	if err := agent(api.MethodShareSyncCall, watch, nil); err == nil || !strings.Contains(err.Error(), "was reset") {
		t.Errorf("a round after a reset = %v", err)
	}

	closeAgent()
	deadline := time.Now().Add(5 * time.Second)
	for len(g.called("sync.unwatch")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the guest's watch was not stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if u := g.called("sync.unwatch"); u[0] != `sync.unwatch {"id":"live"}` {
		t.Errorf("unwatch = %q", u)
	}
	if err := call(api.MethodShareLiveReconcile, api.ShareParams{VM: "dev", Name: "live"}, nil); err == nil || !strings.Contains(err.Error(), "no glov share agent") {
		t.Errorf("reconcile without an agent = %v", err)
	}
}
