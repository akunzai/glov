package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/store"
)

// The Guest Service's files in the guest (docs/guest-protocol.md, "Guest
// Service updates"). The staging directory is on tmpfs, so a reboot during
// an update leaves nothing behind.
const (
	guestInstalled = "/usr/local/bin/glov-guest"
	guestStageDir  = "/run/glov-guest-update"
	guestStaged    = guestStageDir + "/glov-guest"
	// guestChunk is how much one file.write carries, as glov cp sends.
	guestChunk = 512 << 10
	// checkTimeout bounds vm.status's check.
	checkTimeout = 10 * time.Second
)

// guestCheck is a check made on one Guest Service connection.
type guestCheck struct {
	conn  string
	check api.GuestCheck
}

// status is r's state for vm.status, with the Guest Service's update state
// checked once per connection.
func (d *Daemon) status(ctx context.Context, r store.Record) api.VMInfo {
	info := d.info(r)
	gs := info.GuestService
	if gs == nil || gs.State != "ready" || gs.ConnectionID == nil {
		return info
	}
	conn := *gs.ConnectionID
	d.checkMu.Lock()
	c, ok := d.checks[r.ID]
	d.checkMu.Unlock()
	if !ok || c.conn != conn {
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		c = guestCheck{conn, d.check(cctx, r, gs)}
		cancel()
		// An unknown answer is asked again on the next status.
		if c.check.State != api.UpdateUnknown {
			d.remember(r.ID, c)
		}
	}
	gs.Update = &c.check
	return info
}

func (d *Daemon) remember(id string, c guestCheck) {
	d.checkMu.Lock()
	defer d.checkMu.Unlock()
	if d.checks == nil {
		d.checks = map[string]guestCheck{}
	}
	d.checks[id] = c
}

// readyGuest returns r's Guest Service when it answers.
func (d *Daemon) readyGuest(r store.Record) (*api.GuestService, error) {
	state, gs := d.VMs.Info(r.ID)
	switch {
	case state != "running":
		return nil, fmt.Errorf("VM %s is not running: glov vm start %s", r.Name, r.Name)
	case gs == nil || gs.State != "ready" || gs.ConnectionID == nil:
		return nil, fmt.Errorf("VM %s's Guest Service is not ready: glov vm wait %s guest-ready", r.Name, r.Name)
	}
	return gs, nil
}

func (d *Daemon) guestCheck(ctx context.Context, caller string, p api.VMParams) (api.GuestCheck, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return api.GuestCheck{}, err
	}
	gs, err := d.readyGuest(r)
	if err != nil {
		return api.GuestCheck{}, err
	}
	c := d.check(ctx, r, gs)
	if c.State != api.UpdateUnknown {
		d.remember(r.ID, guestCheck{*gs.ConnectionID, c})
	}
	return c, nil
}

// check compares the installed Guest Service with glovd's by SHA-256.
func (d *Daemon) check(ctx context.Context, r store.Record, gs *api.GuestService) api.GuestCheck {
	target := d.Version
	c := api.GuestCheck{State: api.UpdateUnknown, CurrentVersion: gs.Version, TargetVersion: &target}
	_, want, err := d.guestBinary()
	if err == nil {
		var have string
		if have, err = d.guestSHA(ctx, r, guestInstalled); err == nil {
			c.State = api.UpdateAvailable
			if have == want {
				c.State = api.UpdateUpToDate
			}
			return c
		}
	}
	p := err.Error()
	c.Problem = &p
	return c
}

// guestBinary is glovd's Guest Service and its SHA-256.
func (d *Daemon) guestBinary() ([]byte, string, error) {
	bin, err := d.GuestBinary()
	if err != nil {
		return nil, "", fmt.Errorf("glovd's Guest Service: %w", err)
	}
	sum := sha256.Sum256(bin)
	return bin, hex.EncodeToString(sum[:]), nil
}

// guestSHA is the SHA-256 of the guest's file at path.
func (d *Daemon) guestSHA(ctx context.Context, r store.Record, path string) (string, error) {
	var out []string
	code, err := d.runGuest(ctx, r, []string{"sha256sum", path}, func(line string) { out = append(out, line) })
	if err != nil {
		return "", err
	}
	text := strings.Join(out, "\n")
	if code != 0 {
		return "", fmt.Errorf("sha256sum %s exited %d: %q", path, code, text)
	}
	sum, _, _ := strings.Cut(text, " ")
	if _, err := hex.DecodeString(sum); err != nil || len(sum) != sha256.Size*2 {
		return "", fmt.Errorf("sha256sum %s printed %q", path, text)
	}
	return sum, nil
}

// busy lists the work a Guest Service restart would cut off.
func (d *Daemon) busy(r store.Record) []string {
	var busy []string
	if n := d.VMs.ExecRunning(r.ID); n > 0 {
		busy = append(busy, count(n, "exec job"))
	}
	d.syncMu.Lock()
	for k, st := range d.syncs {
		if k.vm == r.Name {
			busy = append(busy, "share sync "+st.label)
		}
	}
	d.syncMu.Unlock()
	return busy
}

func count(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// guestUpdate installs glovd's Guest Service on the running VM, as on
// macOS: stage it in tmpfs, check it, let it install itself detached (which
// restarts the service and so ends this connection), then wait for a new
// connection and check the installed file.
func (d *Daemon) guestUpdate(ctx context.Context, caller string, p api.GuestUpdateParams) (api.GuestUpdated, error) {
	if p.Timeout == 0 {
		p.Timeout = api.DefaultGuestUpdateTimeout
	}
	if p.Timeout < 0 || p.Timeout > api.MaxGuestUpdateTimeout {
		return api.GuestUpdated{}, proto.Errorf(proto.CodeInvalidParams, "the timeout must be more than 0 and at most %d seconds", api.MaxGuestUpdateTimeout)
	}
	r, unlock, err := d.locked(caller, p.VM)
	if err != nil {
		return api.GuestUpdated{}, err
	}
	defer unlock()
	gs, err := d.readyGuest(r)
	if err != nil {
		return api.GuestUpdated{}, err
	}
	from := "unknown"
	if gs.Version != nil {
		from = *gs.Version
	}
	bin, want, err := d.guestBinary()
	if err != nil {
		return api.GuestUpdated{}, err
	}
	have, err := d.guestSHA(ctx, r, guestInstalled)
	if err != nil {
		return api.GuestUpdated{}, err
	}
	if have == want {
		return api.GuestUpdated{From: from, To: from}, nil
	}
	if busy := d.busy(r); len(busy) > 0 && !p.Force {
		return api.GuestUpdated{}, proto.Errorf(proto.CodeInvalidParams,
			"VM %s is busy (%s), which restarting the Guest Service cuts off: wait for it, or glov vm guest update %s --force",
			r.Name, strings.Join(busy, ", "), r.Name)
	}
	before := *gs.ConnectionID
	if err := d.stageGuest(ctx, r, bin, want); err != nil {
		return api.GuestUpdated{}, err
	}
	var out []string
	code, err := d.runGuest(ctx, r, launchInstall(), func(line string) { out = append(out, line) })
	if err == nil && code != 0 {
		err = fmt.Errorf("starting the install exited %d: %q", code, strings.Join(out, "\n"))
	}
	if err != nil {
		d.cleanStage(ctx, r)
		return api.GuestUpdated{}, err
	}
	slog.Info("Guest Service update started", "vm", r.Name, "from", from)
	gs, err = d.waitNewGuest(ctx, r, before, time.Duration(p.Timeout*float64(time.Second)))
	if err != nil {
		return api.GuestUpdated{}, err
	}
	// The jobs --force cut off went with the old Guest Service.
	d.VMs.ExecLost(r.ID)
	defer d.cleanStage(ctx, r)
	if have, err = d.guestSHA(ctx, r, guestInstalled); err != nil {
		return api.GuestUpdated{}, err
	}
	if have != want {
		return api.GuestUpdated{}, fmt.Errorf("VM %s's Guest Service restarted, but the installed file is not glovd's (SHA-256 %s, want %s)", r.Name, have, want)
	}
	d.remember(r.ID, guestCheck{*gs.ConnectionID, api.GuestCheck{State: api.UpdateUpToDate, CurrentVersion: gs.Version, TargetVersion: &d.Version}})
	to := "unknown"
	if gs.Version != nil {
		to = *gs.Version
	}
	slog.Info("Guest Service updated", "vm", r.Name, "from", from, "to", to)
	return api.GuestUpdated{Updated: true, From: from, To: to}, nil
}

// stageGuest writes bin to guestStaged, makes it executable and checks it.
func (d *Daemon) stageGuest(ctx context.Context, r store.Record, bin []byte, want string) error {
	for off := 0; off == 0 || off < len(bin); off += guestChunk {
		end := min(off+guestChunk, len(bin))
		params := map[string]any{"path": guestStaged, "offset": off, "data": bin[off:end], "mkdirs": true}
		if off == 0 {
			params["truncate"] = true
		}
		if err := d.VMs.GuestCall(ctx, r.ID, "file.write", params, nil); err != nil {
			d.cleanStage(ctx, r)
			return guestErr(r, fmt.Errorf("staging the Guest Service: %w", err))
		}
	}
	code, err := d.runGuest(ctx, r, []string{"chmod", "755", guestStaged}, func(string) {})
	if err == nil && code != 0 {
		err = fmt.Errorf("chmod exited %d", code)
	}
	var have string
	if err == nil {
		have, err = d.guestSHA(ctx, r, guestStaged)
	}
	if err == nil && have != want {
		err = fmt.Errorf("the staged Guest Service has SHA-256 %s, want %s", have, want)
	}
	if err != nil {
		d.cleanStage(ctx, r)
		return fmt.Errorf("VM %s: staging the Guest Service: %w", r.Name, err)
	}
	return nil
}

// cleanStage removes the staging directory, best effort.
func (d *Daemon) cleanStage(ctx context.Context, r store.Record) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if code, err := d.runGuest(ctx, r, []string{"rm", "-rf", guestStageDir}, func(string) {}); err != nil || code != 0 {
		slog.Warn("removing the Guest Service staging directory", "vm", r.Name, "code", code, "err", err)
	}
}

// waitNewGuest waits until a Guest Service connection other than before is
// ready.
func (d *Daemon) waitNewGuest(ctx context.Context, r store.Record, before string, timeout time.Duration) (*api.GuestService, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		state, gs := d.VMs.Info(r.ID)
		if state != "running" {
			return nil, fmt.Errorf("VM %s stopped during the Guest Service update", r.Name)
		}
		if gs != nil && gs.State == "ready" && gs.ConnectionID != nil && *gs.ConnectionID != before {
			return gs, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("VM %s's new Guest Service did not answer within %s: check glov vm status %s, then glov vm reboot %s", r.Name, timeout, r.Name, r.Name)
			}
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// launchInstall starts the staged Guest Service's install outside the Guest
// Service, so it outlives the service restart it causes: a transient unit
// runs it, since stopping glov-guest.service kills every process in its
// cgroup; Debian mounts /run noexec, so the unit runs a copy next to the
// installed binary and removes it after.
func launchInstall() []string {
	next := guestInstalled + ".next"
	return []string{"systemd-run", "--no-block", "--collect", "--unit", "glov-guest-update", "/bin/sh", "-c",
		`install -m 0755 "$0" "$1" && "$1" install; rc=$?; rm -f "$1"; exit $rc`, guestStaged, next}
}
