package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/live"
	"github.com/akunzai/glov/windows/internal/share"
	"golang.org/x/sys/windows"
)

// The agent's pacing: changes are gathered for liveSettle before a round,
// a full round runs every liveFullEvery, and a path that failed is tried
// again after liveRetry, liveTries times in a row.
const (
	liveSettle    = 200 * time.Millisecond
	liveFullEvery = 5 * time.Minute
	liveRetry     = 2 * time.Second
	liveTries     = 5
	// liveStaleLock is how long a git lock file holds the share's git
	// data before share status names it: one git left behind.
	liveStaleLock = time.Minute
	// liveVMPoll is how often the agent asks whether the VM still runs
	// and still has the share: one that stopped or was deleted, or a share
	// removed, with nothing changing would hold the folder, and its log,
	// open for good.
	liveVMPoll = 2 * time.Second
	// liveLogMax is how large an agent's log grows before it starts anew
	// (the previous one kept as .old).
	liveLogMax = 4 << 20
)

// liveFiles are where glov keeps a live share's state and its agent's log:
// %LOCALAPPDATA%\glov\vms\<vm>\sync\<name>.live.json and .live.log.
func liveFiles(vm, name string) (state, logFile string, err error) {
	dir, err := syncCacheDir(vm)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, name+".live.json"), filepath.Join(dir, name+".live.log"), nil
}

// liveReconnect is how long an agent whose glovd went away (restarted by
// glov host install, say) keeps trying to take its share again.
const liveReconnect = 2 * time.Minute

// errVMReset ends a session whose VM glov vm reset gave another disk: the
// next session takes the share again.
var errVMReset = errors.New("VM reset")

// liveState asks glovd for vm's state. gone says why the agent of vm's
// share sh has nothing left to keep in step: the VM stopped or was deleted,
// or the share was removed (or added again with another folder or mode).
// err is glovd not answering, which says neither.
func liveState(ctx context.Context, c caller, vm string, sh api.ShareInfo) (info api.VMInfo, gone, err error) {
	// c.Call, not call: a VM or share no longer there is a *proto.Error,
	// which call turns into its bare message. A VM stopped and deleted
	// between two polls is only ever seen as not found.
	notFound := func(err error) bool {
		pe, ok := errors.AsType[*proto.Error](err)
		return ok && pe.Code == proto.CodeNotFound
	}
	if err := c.Call(ctx, api.MethodVMStatus, api.VMParams{VM: vm}, &info); notFound(err) {
		return info, fmt.Errorf("VM %s deleted", vm), nil
	} else if err != nil {
		return info, nil, err
	}
	if info.State == "stopped" {
		return info, fmt.Errorf("VM %s stopped", vm), nil
	}
	var shares []api.ShareInfo
	if err := c.Call(ctx, api.MethodShareList, api.VMParams{VM: vm}, &shares); notFound(err) {
		return info, fmt.Errorf("VM %s deleted", vm), nil
	} else if err != nil {
		return info, nil, err
	}
	if !slices.ContainsFunc(shares, func(s api.ShareInfo) bool {
		return s.Name == sh.Name && s.HostPath == sh.HostPath && s.Mode == sh.Mode
	}) {
		return info, fmt.Errorf("share %s of VM %s removed", sh.Name, vm), nil
	}
	return info, nil, nil
}

// runShareAgent keeps a live share in step until the VM stops or is
// deleted, the share is removed, another agent holds it, or glovd stays
// away past liveReconnect (glov share agent, which glov starts in the
// background; see startShareAgent).
func runShareAgent(vm, name string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("vm", vm, "share", name)
	var lostAt time.Time
	for {
		began, lost, err := agentSession(ctx, vm, name, log)
		if began {
			lostAt = time.Time{}
		}
		if !lost || ctx.Err() != nil {
			return err
		}
		if lostAt.IsZero() {
			lostAt = time.Now()
			// After a reset (logged by agentSession), wait too: glovd
			// lets the share go only once it sees this connection close.
			if !errors.Is(err, errVMReset) {
				log.Info("glovd went away; taking the share again once it is back", "err", err)
			}
		}
		if time.Since(lostAt) > liveReconnect {
			return fmt.Errorf("glovd has been away for %s: %w", liveReconnect, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(liveRetry):
		}
	}
}

// agentSession keeps the share in step over one connection to glovd. began
// says it took the share; lost that the connection failed (glovd went
// away), so another session may follow.
func agentSession(ctx context.Context, vm, name string, log *slog.Logger) (began, lost bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	agent := &live.Agent{Log: log, Settle: liveSettle, FullEvery: liveFullEvery, Retry: liveRetry, Tries: liveTries, StaleLock: liveStaleLock}
	var rewatch func()
	notify := func(method string, params jsontext.Value) {
		var p api.ShareChanged
		if json.Unmarshal(params, &p) != nil || p.Name != name {
			return
		}
		switch method {
		case api.NotifyShareChanged:
			if p.Error != "" {
				log.Warn("guest watch", "err", p.Error)
			}
			if p.Overflow {
				agent.Full()
			} else {
				agent.Changed(p.Paths)
			}
		case api.NotifyShareRewatch:
			go rewatch()
		case api.NotifyShareReconcile:
			agent.Full()
		}
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := dialGlovd(dialCtx, notify)
	dialCancel()
	if err != nil {
		return false, true, err
	}
	defer c.Close()
	go func() {
		<-c.Done()
		cancel()
	}()
	defer func() {
		select {
		case <-c.Done():
			lost = true
		default:
		}
	}()
	var begun api.ShareSyncBegun
	if err := call(ctx, c, api.MethodShareLiveBegin, api.ShareParams{VM: vm, Name: name}, &begun); err != nil {
		return false, false, err
	}
	began = true
	vmGone := make(chan error, 1)
	// reset: glov vm reset gave the VM another disk, so the share is taken
	// again, against an empty base (live.Load).
	var reset atomic.Bool
	go func() {
		t := time.NewTicker(liveVMPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			info, gone, err := liveState(ctx, c, begun.VM, begun.Share)
			if gone != nil {
				vmGone <- gone
			} else if err == nil && info.Disk != begun.Disk {
				reset.Store(true)
			} else {
				continue
			}
			cancel()
			return
		}
	}()
	vm, sh := begun.VM, begun.Share
	ex, err := share.CompileExcludes(sh.Exclude)
	if err != nil {
		return began, false, err
	}
	hostPath, err := share.Canonical(sh.HostPath)
	if err != nil {
		return began, false, fmt.Errorf("the host folder of share %s: %w", name, err)
	}
	if !strings.EqualFold(hostPath, sh.HostPath) {
		return began, false, fmt.Errorf("the host folder of share %s, %s, is now %s (remove and add the share again)", name, sh.HostPath, hostPath)
	}
	root, err := os.OpenRoot(hostPath)
	if err != nil {
		return began, false, err
	}
	defer root.Close()
	stateFile, _, err := liveFiles(vm, name)
	if err != nil {
		return began, false, err
	}
	base, keys, err := live.Load(stateFile, hostPath, sh.GuestPath, begun.Disk)
	if err != nil {
		return began, false, err
	}
	guestCall := func(method string, params, result any) error {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		return call(ctx, c, api.MethodShareSyncCall, api.ShareSyncCallParams{VM: vm, Name: name, Method: method, Params: b}, result)
	}
	rules := ex.Rules
	if rules == nil {
		rules = []share.Rule{}
	}
	watch := func() error {
		var info api.VMInfo
		if err := call(ctx, c, api.MethodVMWait, api.WaitParams{VM: vm, State: api.WaitGuestReady, Timeout: createWaitTimeout}, &info); err != nil {
			return err
		}
		return guestCall("sync.watch", map[string]any{"exclude": rules}, nil)
	}
	rewatch = func() {
		if err := watch(); err != nil {
			log.Warn("guest watch not restarted", "err", err)
			return
		}
		agent.Full()
	}
	if err := watch(); err != nil {
		return began, false, err
	}
	hw, err := live.WatchHost(hostPath, agent.Changed, agent.Full)
	if err != nil {
		return began, false, err
	}
	defer hw.Close()
	host := live.NewHost(root, ex, keys)
	agent.Engine = &live.Engine{Host: host, Guest: live.NewGuest(guestCall, rules), Base: base, GitDirs: sh.GitDirs}
	agent.Save = func() error {
		return live.Save(stateFile, hostPath, sh.GuestPath, begun.Disk, agent.Engine.Base, host.Keys)
	}
	agent.Conflict = func(cf live.Conflict) {
		log.Warn("conflict", "path", cf.Path, "kept", cf.Winner, "why", cf.Why, "branch", cf.Kept)
		call(ctx, c, api.MethodShareLiveConflict, api.ShareLiveConflictParams{VM: vm, Name: name, Path: cf.Path, Winner: cf.Winner, Why: cf.Why, Kept: cf.Kept}, nil)
	}
	agent.InStep = func() {
		call(ctx, c, api.MethodShareSyncProgress, api.ShareSyncProgressParams{VM: vm, Name: name, Stage: api.StageLive, InStep: true}, nil)
	}
	agent.Stale = func(locks []string) {
		call(ctx, c, api.MethodShareLiveLocks, api.ShareLiveLocksParams{VM: vm, Name: name, Locks: locks}, nil)
	}
	agent.Recover = func(error) (bool, error) {
		info, gone, err := liveState(ctx, c, vm, sh)
		if gone != nil {
			return false, gone
		}
		if err != nil {
			return false, nil
		}
		if info.Disk != begun.Disk {
			reset.Store(true)
			cancel()
			return false, nil
		}
		if info.GuestService != nil && info.GuestService.State == "ready" {
			return false, nil
		}
		log.Info("waiting for the Guest Service")
		if err := watch(); err != nil {
			return false, nil
		}
		return true, nil
	}
	log.Info("live share started", "hostPath", hostPath, "guestPath", sh.GuestPath)
	err = agent.Run(ctx)
	select {
	case err = <-vmGone:
	default:
	}
	if reset.Load() {
		log.Info("VM reset: taking the share again")
		return began, true, errVMReset
	}
	log.Info("live share stopped", "err", err)
	return began, false, err
}

// startShareAgent starts glov share agent for a live share in the
// background, detached from this console, logging to the share's agent
// log. An agent already running for it keeps the share, and the new one
// exits.
func startShareAgent(vm, name string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_, logFile, err := liveFiles(vm, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return err
	}
	if fi, err := os.Stat(logFile); err == nil && fi.Size() > liveLogMax {
		os.Rename(logFile, logFile+".old")
	}
	out, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	start := func(flags uint32) error {
		cmd := exec.Command(exe, "share", "agent", vm, name)
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	// Out of the console's job, so that closing the terminal leaves it
	// running; a job that allows no breakaway keeps it.
	const detached = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
	if err := start(detached | windows.CREATE_BREAKAWAY_FROM_JOB); err != nil {
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return err
		}
		return start(detached)
	}
	return nil
}

// ensureShareAgents starts the agents of vm's live shares that run none.
func ensureShareAgents(ctx context.Context, c caller, vm string) {
	var list []api.ShareInfo
	var running []api.SyncProgress
	if call(ctx, c, api.MethodShareList, api.VMParams{VM: vm}, &list) != nil ||
		call(ctx, c, api.MethodShareStatus, api.ShareStatusParams{VM: vm}, &running) != nil {
		return
	}
	for _, s := range list {
		if s.Mode != share.Live || slices.ContainsFunc(running, func(p api.SyncProgress) bool { return p.Share == s.Name && p.Stage == api.StageLive }) {
			continue
		}
		if err := startShareAgent(vm, s.Name); err != nil {
			fmt.Fprintf(os.Stderr, "live share %s not kept in step: %v\n", s.Name, err)
		}
	}
}
