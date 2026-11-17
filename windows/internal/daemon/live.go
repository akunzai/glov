package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/share"
)

// maxConflicts bounds the conflicts share.status shows for a live share.
const maxConflicts = 20

// liveBegin takes a live share for the connection of the glov share agent
// that keeps it in step, until that connection ends. glovd passes the
// guest's changes on to it (guestNotify) and tells it when the Guest
// Service reconnects (liveReady).
func (d *Daemon) liveBegin(conn *syncConn, notify func(string, any) error, caller string, p api.ShareParams) (api.ShareSyncBegun, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return api.ShareSyncBegun{}, err
	}
	s, err := share.Find(r.Shares, p.Name)
	if err != nil {
		return api.ShareSyncBegun{}, proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	if s.Mode != share.Live {
		return api.ShareSyncBegun{}, fmt.Errorf("share %s of VM %s is not live (mode %s): glov share sync %s %s copies it into the guest", s.Name, r.Name, s.Mode, r.Name, s.Name)
	}
	if !d.VMs.Running(r.ID) {
		return api.ShareSyncBegun{}, fmt.Errorf("VM %s is not running: glov vm start %s", r.Name, r.Name)
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	k := syncKey{r.Name, s.Name}
	if st := d.syncs[k]; st != nil {
		if st.live {
			return api.ShareSyncBegun{}, fmt.Errorf("share %s of VM %s is already kept in step by another glov share agent", s.Name, r.Name)
		}
		return api.ShareSyncBegun{}, fmt.Errorf("a sync of share %s is already running on VM %s", s.Name, r.Name)
	}
	if d.syncs == nil {
		d.syncs = map[syncKey]*syncState{}
	}
	d.syncs[k] = &syncState{conn: conn, id: r.ID, label: s.Name, root: s.GuestPath(), stage: api.StageLive, started: time.Now(), live: true, notify: notify, disk: r.Disk}
	slog.Info("live share started", "vm", r.Name, "share", s.Name)
	return api.ShareSyncBegun{VM: r.Name, Share: api.NewShareInfo(s), Disk: r.Disk}, nil
}

// liveHolder returns the live hold of vm's share name, nil if none or if
// its agent went away.
func (d *Daemon) liveHolder(vm, name string) *syncState {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	if st := d.syncs[syncKey{vm, name}]; st != nil && st.live && !st.ending {
		return st
	}
	return nil
}

// guestNotify passes a guest's sync.changed on to the agent holding that
// share of VM vm. It runs on the guest session's read loop, so the agent's
// connection is written to beside it.
func (d *Daemon) guestNotify(vm, method string, params jsontext.Value) {
	if method != "sync.changed" {
		return
	}
	var c struct {
		ID       string   `json:"id"`
		Paths    []string `json:"paths"`
		Overflow bool     `json:"overflow"`
		Error    string   `json:"error"`
	}
	if json.Unmarshal(params, &c) != nil {
		return
	}
	st := d.liveHolder(vm, c.ID)
	if st == nil {
		return
	}
	if c.Error != "" {
		slog.Warn("live share watch", "vm", vm, "share", c.ID, "err", c.Error)
	}
	go st.notify(api.NotifyShareChanged, api.ShareChanged{VM: vm, Name: c.ID, Paths: c.Paths, Overflow: c.Overflow, Error: c.Error})
}

// liveReady tells the agents of vm's live shares that the Guest Service
// (re)connected: its watches are gone.
func (d *Daemon) liveReady(vm string) {
	d.syncMu.Lock()
	var held []*syncState
	var names []string
	for k, st := range d.syncs {
		if k.vm == vm && st.live && !st.ending {
			held, names = append(held, st), append(names, k.share)
		}
	}
	d.syncMu.Unlock()
	for i, st := range held {
		go st.notify(api.NotifyShareRewatch, api.ShareParams{VM: vm, Name: names[i]})
	}
}

// liveConflict logs a path both sides of a live share changed and keeps it
// for share.status.
func (d *Daemon) liveConflict(conn *syncConn, caller string, p api.ShareLiveConflictParams) error {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return err
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	st := d.syncs[syncKey{r.Name, p.Name}]
	if st == nil || st.conn != conn || !st.live {
		return fmt.Errorf("share %s of VM %s is not kept in step on this connection", p.Name, r.Name)
	}
	p.VM = r.Name
	slog.Warn("live share conflict", "vm", r.Name, "share", p.Name, "path", p.Path, "kept", p.Winner, "why", p.Why)
	st.conflicts = append(st.conflicts, p)
	if n := len(st.conflicts); n > maxConflicts {
		st.conflicts = st.conflicts[n-maxConflicts:]
	}
	return nil
}

// liveLocks keeps for share.status the git lock files that hold a live
// share's git data.
func (d *Daemon) liveLocks(conn *syncConn, caller string, p api.ShareLiveLocksParams) error {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return err
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	st := d.syncs[syncKey{r.Name, p.Name}]
	if st == nil || st.conn != conn || !st.live {
		return fmt.Errorf("share %s of VM %s is not kept in step on this connection", p.Name, r.Name)
	}
	if len(p.Locks) > 0 {
		slog.Warn("live share git locks", "vm", r.Name, "share", p.Name, "locks", p.Locks)
	}
	st.locks = p.Locks
	return nil
}

// liveReconcile asks the agent of a live share for a full round.
func (d *Daemon) liveReconcile(caller string, p api.ShareParams) error {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return err
	}
	st := d.liveHolder(r.Name, p.Name)
	if st == nil {
		return proto.Errorf(proto.CodeNotFound, "no glov share agent keeps share %s of VM %s in step", p.Name, r.Name)
	}
	return st.notify(api.NotifyShareReconcile, api.ShareParams{VM: r.Name, Name: p.Name})
}

// unwatchGuest stops the guest's watch of a live share whose agent went
// away. An older Guest Service does not know it.
func (d *Daemon) unwatchGuest(id, name string) {
	withTimeout(context.Background(), 5*time.Second, func(ctx context.Context) error {
		return d.VMs.GuestCall(ctx, id, "sync.unwatch", map[string]string{"id": name}, nil)
	})
}
