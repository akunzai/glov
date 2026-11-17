package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/share"
	"github.com/akunzai/glov/windows/internal/store"
)

// fakeGuestSync records the sync calls the daemon makes and answers them.
type fakeGuestSync struct {
	mu    sync.Mutex
	calls []string // method + " " + params as JSON
	// manifest answers sync.manifest; nil answers an empty listing.
	manifest func(ctx context.Context) (any, error)
	status   any
	// echoOwner answers sync.apply with the owner it was given, as a
	// Guest Service that applies it does.
	echoOwner bool
}

func (g *fakeGuestSync) answer(ctx context.Context, method string, params, result any) error {
	b, _ := json.Marshal(params, json.Deterministic(true))
	g.mu.Lock()
	g.calls = append(g.calls, method+" "+string(b))
	status := g.status
	g.mu.Unlock()
	var res any = map[string]any{}
	switch method {
	case "sync.manifest":
		g.mu.Lock()
		echo := g.echoOwner
		g.mu.Unlock()
		res = map[string]any{"entries": []any{}, "total": 0, "owner": g.owner(params, echo)}
		if g.manifest != nil {
			var err error
			if res, err = g.manifest(ctx); err != nil {
				return err
			}
		}
	case "sync.apply":
		g.mu.Lock()
		echo := g.echoOwner
		g.mu.Unlock()
		res = map[string]any{"failures": []any{}, "owner": g.owner(params, echo)}
	case "sync.status":
		res = status
		if res == nil {
			res = map[string]any{"busy": false}
		}
	}
	if result == nil {
		return nil
	}
	out, _ := json.Marshal(res)
	return json.Unmarshal(out, result)
}

// owner is the owner in params when echo is set.
func (g *fakeGuestSync) owner(params any, echo bool) string {
	var owner string
	if p, ok := params.(map[string]jsontext.Value); ok && echo {
		json.Unmarshal(p["owner"], &owner)
	}
	return owner
}

func (g *fakeGuestSync) called(prefix string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// syncing serves dev, running with a share proj, and the fake guest.
func syncing(t *testing.T, watch SyncWatch) (*fakeRunner, *fakeGuestSync, func(method string, params, result any) error) {
	t.Helper()
	f, call := lifecycle(t, func(d *Daemon) { d.Watch = watch })
	g := &fakeGuestSync{}
	f.guestSync = g.answer
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(api.MethodShareAdd, api.ShareAddParams{VM: "dev", Share: share.Share{Name: "proj", HostPath: `C:\src\Proj`}}, nil); err != nil {
		t.Fatal(err)
	}
	return f, g, call
}

func secondClient(t *testing.T, f *fakeRunner) func(method string, params, result any) error {
	t.Helper()
	me := testOwner
	c, err := dial(t, f.pipe, me)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return func(method string, params, result any) error { return c.Call(t.Context(), method, params, result) }
}

func TestShareSyncPassesCallsOnForTheConnectionThatHoldsTheShare(t *testing.T) {
	f, g, call := syncing(t, SyncWatch{})
	other := secondClient(t, f)
	manifest := api.ShareSyncCallParams{VM: "dev", Name: "proj", Method: "sync.manifest", Params: jsontext.Value(`{"subdir":"","id":"x","root":"/etc"}`)}
	if err := call(api.MethodShareSyncCall, manifest, nil); err == nil || !strings.Contains(err.Error(), "share.syncBegin first") {
		t.Errorf("syncCall before begin = %v", err)
	}
	var begun api.ShareSyncBegun
	if err := call(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "de", Name: "proj", Label: "proj/src"}, &begun); err != nil {
		t.Fatal(err)
	}
	if begun.VM != "dev" || begun.Share.GuestPath != "/mnt/c/src/Proj" {
		t.Errorf("begun = %+v", begun)
	}
	if err := other(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "proj"}, nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a second sync = %v", err)
	}
	if err := other(api.MethodShareSyncCall, manifest, nil); err == nil {
		t.Error("another connection's call was passed on")
	}
	var res jsontext.Value
	if err := call(api.MethodShareSyncCall, manifest, &res); err != nil {
		t.Fatal(err)
	}
	if got := g.called("sync.manifest"); len(got) != 1 || got[0] != `sync.manifest {"id":"proj","root":"/mnt/c/src/Proj","subdir":""}` {
		t.Errorf("guest calls = %q", got)
	}
	bad := manifest
	bad.Method = "file.read"
	if err := call(api.MethodShareSyncCall, bad, nil); err == nil {
		t.Error("file.read passed on")
	}

	if err := call(api.MethodShareSyncProgress, api.ShareSyncProgressParams{VM: "dev", Name: "proj", Stage: api.StageApplying, SentBytes: 5, TotalBytes: 9}, nil); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.status = map[string]any{"busy": true, "phase": "apply", "path": "a.txt", "files": 1, "bytes": 5, "seconds": 1.5}
	g.mu.Unlock()
	var status []api.SyncProgress
	if err := other(api.MethodShareStatus, api.ShareStatusParams{VM: "dev"}, &status); err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || status[0].Share != "proj/src" || status[0].Stage != api.StageApplying || status[0].SentBytes != 5 || status[0].TotalBytes != 9 ||
		status[0].Guest == nil || status[0].Guest.Path != "a.txt" || status[0].Guest.Bytes != 5 {
		t.Errorf("share.status = %+v", status)
	}
	if err := other(api.MethodShareStatus, api.ShareStatusParams{VM: "dev", Name: "nope"}, &status); err == nil {
		t.Error("status of a missing share")
	}

	if err := call(api.MethodShareSyncEnd, api.ShareSyncEndParams{VM: "dev", Name: "proj"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := g.called("sync.cancel"); len(got) != 0 {
		t.Errorf("an ended sync was cancelled: %q", got)
	}
	if err := other(api.MethodShareStatus, api.ShareStatusParams{VM: "dev"}, &status); err != nil || len(status) != 0 {
		t.Errorf("share.status after the end = %+v, %v", status, err)
	}
	if err := other(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "proj"}, nil); err != nil {
		t.Errorf("begin after the end = %v", err)
	}
}

func TestShareSyncOfAGlovThatGoesAwayIsCancelledInTheGuest(t *testing.T) {
	f, g, call := syncing(t, SyncWatch{})
	me := testOwner
	c, err := dial(t, f.pipe, me)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Call(t.Context(), api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "proj"}, nil); err != nil {
		t.Fatal(err)
	}
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := call(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "proj"}, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the share stayed held: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := g.called("sync.cancel"); len(got) != 1 || got[0] != `sync.cancel {"id":"proj"}` {
		t.Errorf("cancel calls = %q", got)
	}
}

func TestShareSyncStopsAGuestListingThatMakesNoProgress(t *testing.T) {
	for name, c := range map[string]struct {
		status any
		stall  time.Duration
		want   string
	}{
		"stuck file": {map[string]any{"busy": true, "phase": "manifest", "path": "big.bin", "files": 3, "bytes": 7}, 60 * time.Millisecond, "made no progress for 60ms reading big.bin"},
		// Three idle polls must come before the stall check, which a slow runner can stretch past 60ms.
		"lost reply": {map[string]any{"busy": false}, 10 * time.Second, "not listing, yet its listing never reached the host"},
	} {
		t.Run(name, func(t *testing.T) {
			_, g, call := syncing(t, SyncWatch{Interval: 10 * time.Millisecond, Stall: c.stall})
			g.status = c.status
			g.manifest = func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() }
			if err := call(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "proj"}, nil); err != nil {
				t.Fatal(err)
			}
			err := call(api.MethodShareSyncCall, api.ShareSyncCallParams{VM: "dev", Name: "proj", Method: "sync.manifest", Params: jsontext.Value(`{}`)}, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "share sync of proj to VM dev stopped") {
				t.Errorf("err = %v, want %q", err, c.want)
			}
			if got := g.called("sync.cancel"); len(got) != 1 {
				t.Errorf("cancel calls = %q", got)
			}
		})
	}
}

func TestShareSyncIntoAHarnessVMOwnsFilesAsItsUser(t *testing.T) {
	f, g, call := syncing(t, SyncWatch{})
	me := testOwner
	if err := call(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: "dev", Name: "proj"}, nil); err != nil {
		t.Fatal(err)
	}
	apply := api.ShareSyncCallParams{VM: "dev", Name: "proj", Method: "sync.apply", Params: jsontext.Value(`{"ops":[],"owner":"root"}`)}
	if err := call(api.MethodShareSyncCall, apply, nil); err != nil {
		t.Fatal(err)
	}
	if got := g.called("sync.apply"); len(got) != 1 || strings.Contains(got[0], "owner") {
		t.Errorf("a VM without harnesses: guest calls = %q, want no owner (glov's dropped)", got)
	}

	r, _ := f.st.Load(me, "dev")
	r.Harnesses = []string{"claude"}
	if err := f.st.Save(r); err != nil {
		t.Fatal(err)
	}
	// A Guest Service that ignores the owner leaves root's files: refused.
	if err := call(api.MethodShareSyncCall, apply, nil); err == nil || !strings.Contains(err.Error(), "glov vm guest update dev") {
		t.Errorf("an owner the guest ignored err = %v", err)
	}
	g.mu.Lock()
	g.echoOwner = true
	g.mu.Unlock()
	if err := call(api.MethodShareSyncCall, apply, nil); err != nil {
		t.Fatal(err)
	}
	if got := g.called("sync.apply"); len(got) != 3 || !strings.Contains(got[2], `"owner":"glov"`) {
		t.Errorf("a harness VM: guest calls = %q, want owner glov", got)
	}
	manifest := apply
	manifest.Method, manifest.Params = "sync.manifest", jsontext.Value(`{}`)
	if err := call(api.MethodShareSyncCall, manifest, nil); err != nil {
		t.Fatal(err)
	}
	if got := g.called("sync.manifest"); len(got) != 1 || !strings.Contains(got[0], `"owner":"glov"`) {
		t.Errorf("sync.manifest calls = %q, want owner glov", got)
	}
	// Caught at the listing, before anything is written; a frozen VM is
	// told to keep the update in its template.
	g.mu.Lock()
	g.echoOwner = false
	g.mu.Unlock()
	r.Template = &store.Template{}
	f.st.Save(r)
	if err := call(api.MethodShareSyncCall, manifest, nil); err == nil || !strings.Contains(err.Error(), "glov vm harness install dev") {
		t.Errorf("a manifest whose owner the guest ignored err = %v", err)
	}
}
