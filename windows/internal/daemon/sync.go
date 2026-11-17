package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/share"
	"github.com/akunzai/glov/windows/internal/store"
)

// syncConn identifies the control connection that holds a share's sync. It
// has a field because pointers to distinct zero-size values may be equal.
type syncConn struct{ _ byte }

type syncKey struct{ vm, share string }

// syncState is a running share sync: one per share, held by the connection
// of the glov that runs it.
type syncState struct {
	conn    *syncConn
	id      string // the VM's ID
	label   string
	root    string // the share's guest path
	stage   string
	started time.Time
	sent    int64
	total   int64
	// live: a live share's agent (liveBegin), sent the guest's changes
	// through notify; conflicts are the latest it reported.
	live bool
	// disk is the VM's disk (store.Record.Disk) a live share was taken on.
	disk      int
	notify    func(method string, params any) error
	conflicts []api.ShareLiveConflictParams
	locks     []string
	inStep    bool
	// ending: endSyncs is freeing it. The key stays taken until the guest
	// was told to stop, but nothing is sent to its connection any more.
	ending bool
}

// SyncWatch paces the watch over a sync's first guest listing: the guest's
// sync.status is asked every Interval, and the sync stops once nothing moved
// for Stall. The defaults are macOS's.
type SyncWatch struct {
	Interval, Stall time.Duration
}

var defaultSyncWatch = SyncWatch{Interval: 10 * time.Second, Stall: 2 * time.Minute}

func (d *Daemon) syncWatch() SyncWatch {
	if d.Watch.Interval > 0 {
		return d.Watch
	}
	return defaultSyncWatch
}

// guestSyncStatus is sync.status's result.
type guestSyncStatus struct {
	Busy                  bool `json:"busy"`
	api.GuestSyncProgress `json:",inline"`
}

func (d *Daemon) syncBegin(conn *syncConn, caller string, p api.ShareSyncBeginParams) (api.ShareSyncBegun, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return api.ShareSyncBegun{}, err
	}
	s, err := share.Find(r.Shares, p.Name)
	if err != nil {
		return api.ShareSyncBegun{}, proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	if s.Mode == share.Live {
		// A one-way mirror would delete what only the guest has.
		return api.ShareSyncBegun{}, fmt.Errorf("share %s of VM %s is live: its glov share agent keeps it in step, and glov share sync %s %s asks it for a full round", s.Name, r.Name, r.Name, s.Name)
	}
	if !d.VMs.Running(r.ID) {
		return api.ShareSyncBegun{}, fmt.Errorf("VM %s is not running: glov vm start %s", r.Name, r.Name)
	}
	label := p.Label
	if label == "" {
		label = s.Name
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	k := syncKey{r.Name, s.Name}
	if d.syncs[k] != nil {
		return api.ShareSyncBegun{}, fmt.Errorf("a sync of share %s is already running on VM %s", s.Name, r.Name)
	}
	if d.syncs == nil {
		d.syncs = map[syncKey]*syncState{}
	}
	d.syncs[k] = &syncState{conn: conn, id: r.ID, label: label, root: s.GuestPath(), stage: "starting", started: time.Now()}
	return api.ShareSyncBegun{VM: r.Name, Share: api.NewShareInfo(s)}, nil
}

// held returns the sync of r's share name that conn holds.
func (d *Daemon) held(conn *syncConn, r store.Record, name string) (*syncState, error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	st := d.syncs[syncKey{r.Name, name}]
	if st == nil || st.conn != conn {
		return nil, fmt.Errorf("no sync of share %s on VM %s runs on this connection: call %s first", name, r.Name, api.MethodShareSyncBegin)
	}
	return st, nil
}

func (d *Daemon) syncCall(ctx context.Context, conn *syncConn, caller string, p api.ShareSyncCallParams) (jsontext.Value, error) {
	liveOnly := p.Method == "sync.watch" || p.Method == "sync.read" || p.Method == "sync.unwatch"
	if p.Method != "sync.manifest" && p.Method != "sync.apply" && !liveOnly {
		return nil, proto.Errorf(proto.CodeInvalidParams, "share.syncCall passes on only sync.manifest, sync.apply, and for a live share sync.watch, sync.read and sync.unwatch")
	}
	r, err := d.load(caller, p.VM)
	if err != nil {
		return nil, err
	}
	st, err := d.held(conn, r, p.Name)
	if err != nil {
		return nil, err
	}
	if liveOnly && !st.live {
		return nil, proto.Errorf(proto.CodeInvalidParams, "%s is for a live share's agent (%s)", p.Method, api.MethodShareLiveBegin)
	}
	if st.live && st.disk != r.Disk {
		// The agent's base is of the disk before: a round against it would
		// delete on the host what the guest lost with the reset.
		return nil, fmt.Errorf("VM %s was reset since its glov share agent took share %s: the agent takes it again", r.Name, p.Name)
	}
	var params map[string]jsontext.Value
	if err := json.Unmarshal(p.Params, &params); err != nil || params == nil {
		return nil, proto.Errorf(proto.CodeInvalidParams, "share.syncCall params: want an object")
	}
	params["id"], _ = json.Marshal(p.Name)
	params["root"], _ = json.Marshal(st.root)
	// A harness runs as provision.User, who must be able to change what
	// the sync brings in; glovd decides the owner, never glov. The
	// manifest carries it too, so a Guest Service that would ignore it is
	// caught before the first write.
	delete(params, "owner")
	owner := ""
	if len(r.Harnesses) > 0 && (p.Method == "sync.manifest" || p.Method == "sync.apply") {
		owner = provision.User
		params["owner"], _ = json.Marshal(owner)
	}
	var offset int
	json.Unmarshal(params["offset"], &offset)

	var res jsontext.Value
	call := func(ctx context.Context) error { return d.VMs.GuestCall(ctx, r.ID, p.Method, params, &res) }
	switch {
	case p.Method == "sync.apply" || liveOnly:
		err = withTimeout(ctx, 2*time.Minute, call)
	case len(params["paths"]) > 0:
		err = withTimeout(ctx, time.Minute, call)
	case offset > 0:
		err = withTimeout(ctx, time.Minute, call)
	default:
		// The first page walks and hashes the guest tree, which can take
		// long: sync.status tells a long walk from a stuck one.
		err = withTimeout(ctx, time.Hour, func(ctx context.Context) error { return d.watchListing(ctx, r, p.Name, call) })
	}
	if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeNoMethod {
		return nil, fmt.Errorf("the Guest Service of VM %s is too old for share sync (no %s): update glov-guest in the guest", r.Name, p.Method)
	}
	if stalled, ok := errors.AsType[syncStalled](err); ok {
		d.cancelGuestSync(r.ID, p.Name)
		slog.Warn("share sync stopped", "vm", r.Name, "share", st.label, "reason", string(stalled))
		return nil, fmt.Errorf("share sync of %s to VM %s stopped: %s", st.label, r.Name, string(stalled))
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, fmt.Errorf("share sync of %s to VM %s: the guest did not answer %s in time", st.label, r.Name, p.Method)
	}
	if err != nil {
		return nil, guestErr(r, err)
	}
	if owner != "" {
		var applied struct {
			Owner string `json:"owner"`
		}
		if json.Unmarshal(res, &applied); applied.Owner != owner {
			also := ""
			if r.Template != nil {
				also = "; on a frozen VM the update lasts until glov vm reset, so stop it and run glov vm harness install " + r.Name + ", which updates the template's"
			}
			return nil, fmt.Errorf("the Guest Service of VM %s cannot give synced files to %s: glov vm guest update %s, then sync again%s", r.Name, owner, r.Name, also)
		}
	}
	return res, nil
}

func withTimeout(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return f(ctx)
}

// syncStalled says why watchListing gave up.
type syncStalled string

func (s syncStalled) Error() string { return string(s) }

// watchListing runs call while asking the guest how far the share's listing
// has got. It gives up, naming the file the guest is on, once nothing moved
// for the watch's Stall, or as soon as the guest says three times in a row
// that it is idle while the answer is still missing: the request or its
// answer was lost on the way. A Guest Service without sync.status is
// not watched.
func (d *Daemon) watchListing(ctx context.Context, r store.Record, name string, call func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	w := d.syncWatch()
	tick := time.NewTicker(w.Interval)
	defer tick.Stop()
	var last guestSyncStatus
	since, idle := time.Now(), 0
	for {
		select {
		case err := <-done:
			return err
		case <-tick.C:
		}
		var st guestSyncStatus
		err := withTimeout(ctx, 5*time.Second, func(ctx context.Context) error {
			return d.VMs.GuestCall(ctx, r.ID, "sync.status", map[string]string{"id": name}, &st)
		})
		if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeNoMethod {
			return <-done
		}
		if err != nil {
			continue // a busy or briefly silent guest: counts as no progress
		}
		// The guest ends its progress before it answers, and a request slow
		// to arrive finds it idle too: three polls in a row leave time for
		// both, and for a wait on an earlier call's lock (10 s).
		if st.Busy {
			idle = 0
		} else {
			idle++
		}
		if idle >= 3 {
			return syncStalled("the guest is not listing, yet its listing never reached the host; sync again, and if this repeats, run glov vm reboot " + r.Name)
		}
		moved := st.Phase != last.Phase || st.Path != last.Path || st.Files != last.Files || st.Bytes != last.Bytes
		if moved {
			last, since = st, time.Now()
		} else if time.Since(since) >= w.Stall {
			if st.Busy && st.Path != "" {
				return syncStalled(fmt.Sprintf("the guest made no progress for %v reading %s (another program may hold it)", w.Stall, st.Path))
			}
			return syncStalled(fmt.Sprintf("the guest made no progress for %v", w.Stall))
		}
	}
}

// cancelGuestSync asks the guest to stop the share's listing or batch and
// waits for the answer, so it arrives before a next sync's first call. An
// older Guest Service does not know it.
func (d *Daemon) cancelGuestSync(id, name string) {
	withTimeout(context.Background(), 5*time.Second, func(ctx context.Context) error {
		return d.VMs.GuestCall(ctx, id, "sync.cancel", map[string]string{"id": name}, nil)
	})
}

func (d *Daemon) syncProgress(conn *syncConn, caller string, p api.ShareSyncProgressParams) error {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return err
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	st := d.syncs[syncKey{r.Name, p.Name}]
	if st == nil || st.conn != conn {
		return fmt.Errorf("no sync of share %s on VM %s runs on this connection", p.Name, r.Name)
	}
	if st.live {
		st.inStep = st.inStep || p.InStep
		return nil
	}
	st.stage, st.sent, st.total = p.Stage, p.SentBytes, p.TotalBytes
	return nil
}

func (d *Daemon) syncEnd(conn *syncConn, caller string, p api.ShareSyncEndParams) error {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return err
	}
	d.endSyncs(conn, p.Cancel, func(k syncKey) bool { return k == syncKey{r.Name, p.Name} })
	return nil
}

// endSyncs frees the syncs conn holds that match, after telling the guest to
// stop when cancel is set. glov going away cancels every sync it held.
func (d *Daemon) endSyncs(conn *syncConn, cancel bool, match func(syncKey) bool) {
	d.syncMu.Lock()
	var ended []syncKey
	states := map[syncKey]*syncState{}
	for k, st := range d.syncs {
		if st.conn == conn && match(k) {
			ended = append(ended, k)
			states[k] = st
			st.ending = true
		}
	}
	d.syncMu.Unlock()
	for _, k := range ended {
		st := states[k]
		if st.live {
			d.unwatchGuest(st.id, k.share)
			slog.Info("live share stopped", "vm", k.vm, "share", k.share)
		}
		if cancel {
			d.cancelGuestSync(st.id, k.share)
			slog.Info("share sync cancelled", "vm", k.vm, "share", st.label)
		}
		d.syncMu.Lock()
		if d.syncs[k] == st {
			delete(d.syncs, k)
		}
		d.syncMu.Unlock()
	}
}

// shareStatus lists the VM's running syncs (only name's, unless empty), each
// with the guest's sync.status while the guest works on it. A guest that does
// not answer within 5 s leaves Guest nil.
func (d *Daemon) shareStatus(ctx context.Context, caller string, p api.ShareStatusParams) ([]api.SyncProgress, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return nil, err
	}
	if p.Name != "" {
		if _, err := share.Find(r.Shares, p.Name); err != nil {
			return nil, proto.Errorf(proto.CodeNotFound, "%v", err)
		}
	}
	type running struct {
		name string
		st   syncState
	}
	var list []running
	d.syncMu.Lock()
	for k, st := range d.syncs {
		if k.vm == r.Name && (p.Name == "" || k.share == p.Name) {
			list = append(list, running{k.share, *st})
		}
	}
	d.syncMu.Unlock()
	slices.SortFunc(list, func(a, b running) int { return strings.Compare(a.name, b.name) })
	out := make([]api.SyncProgress, 0, len(list))
	for _, s := range list {
		sp := api.SyncProgress{Share: s.st.label, Stage: s.st.stage, Seconds: time.Since(s.st.started).Seconds(), SentBytes: s.st.sent, TotalBytes: s.st.total,
			Conflicts: s.st.conflicts, Locks: s.st.locks, InStep: s.st.inStep}
		if s.st.stage == api.StageGuestListing || s.st.stage == api.StageApplying {
			var g guestSyncStatus
			err := withTimeout(ctx, 5*time.Second, func(ctx context.Context) error {
				return d.VMs.GuestCall(ctx, r.ID, "sync.status", map[string]string{"id": s.name}, &g)
			})
			if err == nil && g.Busy {
				sp.Guest = &g.GuestSyncProgress
			}
		}
		out = append(out, sp)
	}
	return out, nil
}
