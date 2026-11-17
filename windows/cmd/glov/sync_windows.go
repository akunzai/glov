package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/share"
)

// caller is glovd's control connection (*rpc.Client).
type caller interface {
	Call(ctx context.Context, method string, params, result any) error
}

// sync.apply limits, as on macOS: a call stays well under the 1 MiB frame,
// as data is base64 (4/3) and each op carries its path.
const (
	applyBudget = 900 << 10
	applyChunk  = 512 << 10
	applyMaxOps = 256
	// listLimit bounds each list in a sync's summary; the counts are whole.
	listLimit = 100
)

// syncResult is a sync's summary, as macOS's Results.ShareSync.
type syncResult struct {
	share, guestPath            string
	written, deleted, unchanged int
	bytes                       int64
	hostHashed, guestHashed     int
	writtenPaths, deletedPaths  []string
	overwritten, skipped        []string
	seconds                     float64
}

type syncFailure struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Locked  bool   `json:"locked"`
	Denied  bool   `json:"denied"`
}

// shareSync mirrors a share's host folder, or its subdirectory, into the
// guest. glov reads the host folder as the user; glovd holds the share
// and passes the Guest Service calls on. Ending ctx (Ctrl+C) cancels the
// sync, in the guest too.
type shareSync struct {
	c   caller
	ctx context.Context
	vm  string
	// cacheDir returns where glov keeps the VM's host hash caches.
	cacheDir func(vm string) (string, error)

	name, subdir, label string
	sent, total         int64
}

func (s *shareSync) call(method string, params, result any) error {
	err := s.c.Call(s.ctx, method, params, result)
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return errors.New(pe.Message)
	}
	return err
}

func (s *shareSync) guest(method string, params, result any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.call(api.MethodShareSyncCall, api.ShareSyncCallParams{VM: s.vm, Name: s.name, Method: method, Params: b}, result)
}

func (s *shareSync) progress(stage string) {
	s.call(api.MethodShareSyncProgress, api.ShareSyncProgressParams{VM: s.vm, Name: s.name, Stage: stage, SentBytes: s.sent, TotalBytes: s.total}, nil)
}

func (s *shareSync) run(ref string, verify bool) (res syncResult, err error) {
	started := time.Now()
	if s.name, s.subdir, err = share.ParseRef(ref); err != nil {
		return res, err
	}
	s.label = s.name
	if s.subdir != "" {
		s.label += "/" + s.subdir
	}
	var begun api.ShareSyncBegun
	if err := s.call(api.MethodShareSyncBegin, api.ShareSyncBeginParams{VM: s.vm, Name: s.name, Label: s.label}, &begun); err != nil {
		return res, err
	}
	s.vm = begun.VM
	defer func() {
		cancel := err != nil
		ctx, stop := context.WithTimeout(context.WithoutCancel(s.ctx), 10*time.Second)
		defer stop()
		s.c.Call(ctx, api.MethodShareSyncEnd, api.ShareSyncEndParams{VM: s.vm, Name: s.name, Cancel: cancel}, nil)
		if err != nil && s.ctx.Err() != nil {
			err = fmt.Errorf("share sync of %s to VM %s was cancelled", s.label, s.vm)
		}
	}()
	sh := begun.Share
	ex, err := share.CompileExcludes(sh.Exclude)
	if err != nil {
		return res, err
	}
	if s.subdir != "" && ex.ExcludedOrParent(s.subdir, true) {
		return res, fmt.Errorf("%s is excluded from share %s", s.subdir, s.name)
	}
	// The folder is checked again: it may have gone, or been replaced by
	// a link, since share add.
	hostPath, err := share.Canonical(sh.HostPath)
	if err != nil {
		return res, fmt.Errorf("the host folder of share %s: %w; nothing was synced", s.name, err)
	}
	if !strings.EqualFold(hostPath, sh.HostPath) {
		return res, fmt.Errorf("the host folder of share %s, %s, is now %s; nothing was synced (remove and add the share again)", s.name, sh.HostPath, hostPath)
	}
	root, err := os.OpenRoot(hostPath)
	if err != nil {
		return res, err
	}
	defer root.Close()
	if err := share.ScopeDir(root, s.subdir); err != nil {
		return res, err
	}

	s.progress(api.StageHostScan)
	cacheDir, err := s.cacheDir(s.vm)
	if err != nil {
		return res, err
	}
	host, err := share.Scan(root, s.subdir, ex, filepath.Join(cacheDir, s.name+".json"), verify, func() bool { return s.ctx.Err() != nil })
	if err != nil {
		return res, err
	}
	s.progress(api.StageGuestListing)
	guestFiles, guestDirs, guestHashed, err := s.manifest(ex, verify)
	if err != nil {
		return res, err
	}
	plan := share.NewPlan(host.Files, host.Dirs, guestFiles, guestDirs, ex, host.Skipped)

	ops := []map[string]any{{"op": "mkdir", "path": s.subdir}}
	for _, p := range plan.Deletes {
		ops = append(ops, map[string]any{"op": "delete", "path": p})
	}
	for _, p := range plan.Rmdirs {
		ops = append(ops, map[string]any{"op": "rmdir", "path": p})
	}
	for _, p := range plan.Mkdirs {
		ops = append(ops, map[string]any{"op": "mkdir", "path": p})
	}
	for _, p := range plan.Writes {
		s.total += host.Files[p].Size
	}
	s.progress(api.StageApplying)
	failures, err := s.apply(root, ops, plan.Writes, host.Files)
	if err != nil {
		return res, err
	}

	failed := map[string]bool{}
	for _, f := range failures {
		failed[f.Path] = true
	}
	res = syncResult{
		share: s.label, guestPath: sh.GuestPath, unchanged: plan.Unchanged, bytes: s.sent, hostHashed: host.Hashed, guestHashed: guestHashed,
		overwritten: limit(plan.Overwritten), skipped: limit(host.Skipped), seconds: time.Since(started).Seconds(),
	}
	if s.subdir != "" {
		res.guestPath += "/" + s.subdir
	}
	for _, p := range plan.Writes {
		if !failed[p] {
			res.written++
			res.writtenPaths = appendLimited(res.writtenPaths, p)
		}
	}
	for _, p := range plan.Deletes {
		if !failed[p] {
			res.deleted++
			res.deletedPaths = appendLimited(res.deletedPaths, p)
		}
	}
	if len(failures) > 0 {
		return res, failuresError(s.label, s.vm, failures, res)
	}
	return res, nil
}

func limit(s []string) []string { return s[:min(len(s), listLimit)] }

func appendLimited(s []string, p string) []string {
	if len(s) < listLimit {
		s = append(s, p)
	}
	return s
}

type manifestPage struct {
	Entries []struct {
		Path     string `json:"path"`
		Dir      bool   `json:"dir"`
		Size     int64  `json:"size"`
		Hash     string `json:"hash"`
		Modified bool   `json:"modified"`
	} `json:"entries"`
	Total  int `json:"total"`
	Hashed int `json:"hashed"`
}

// manifest reads the guest copy's listing, page by page.
func (s *shareSync) manifest(ex share.Excludes, verify bool) (map[string]share.GuestFile, map[string]bool, int, error) {
	files, dirs, hashed := map[string]share.GuestFile{}, map[string]bool{}, 0
	rules := ex.Rules
	if rules == nil {
		rules = []share.Rule{}
	}
	for received := 0; ; {
		var page manifestPage
		params := map[string]any{"subdir": s.subdir, "exclude": rules, "verify": verify, "offset": received}
		if err := s.guest("sync.manifest", params, &page); err != nil {
			return nil, nil, 0, err
		}
		if received == 0 {
			hashed = page.Hashed
		}
		for _, e := range page.Entries {
			if e.Dir {
				dirs[e.Path] = true
			} else {
				files[e.Path] = share.GuestFile{Size: e.Size, Hash: e.Hash, Modified: e.Modified}
			}
		}
		received += len(page.Entries)
		if received >= page.Total || len(page.Entries) == 0 {
			return files, dirs, hashed, nil
		}
	}
}

type batch struct {
	ops  []map[string]any
	size int
}

// apply sends ops, then each of writes streamed from the host folder, in
// sync.apply calls of at most applyBudget bytes; the last one commits the
// guest manifest.
func (s *shareSync) apply(root *os.Root, ops []map[string]any, writes []string, files map[string]share.HostFile) ([]syncFailure, error) {
	var b batch
	var failures []syncFailure
	add := func(op map[string]any, cost int) error {
		if len(b.ops) > 0 && (b.size+cost > applyBudget || len(b.ops) >= applyMaxOps) {
			if err := s.flush(&b, false, &failures); err != nil {
				return err
			}
		}
		b.ops = append(b.ops, op)
		b.size += cost
		return nil
	}
	for _, op := range ops {
		if err := add(op, 64+len(op["path"].(string))); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, applyChunk)
	for _, p := range writes {
		f, err := share.OpenFile(root, p)
		if err != nil {
			failures = append(failures, syncFailure{Path: p, Message: "not sent: " + err.Error()})
			continue
		}
		var offset int64
		for {
			if err := s.ctx.Err(); err != nil {
				f.Close()
				return nil, err
			}
			// Up to the room left in this call, so small files share one;
			// a fresh call takes a full chunk.
			want := applyChunk
			if room := applyBudget - b.size - 200 - len(p); room >= 4096 {
				want = min(applyChunk, room*3/4)
			}
			n, rerr := io.ReadFull(f, buf[:want])
			if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
				f.Close()
				return nil, fmt.Errorf("read %s: %w", p, rerr)
			}
			// A short read is the end; a file that ends on a chunk boundary
			// gets an empty last op.
			end := n < want
			op := map[string]any{"op": "write", "path": p, "offset": offset, "data": append([]byte(nil), buf[:n]...)}
			if end {
				op["end"], op["hash"] = true, files[p].Hash
			}
			if err := add(op, 200+len(p)+(n+2)/3*4); err != nil {
				f.Close()
				return nil, err
			}
			offset += int64(n)
			s.sent += int64(n)
			if end {
				break
			}
		}
		f.Close()
	}
	return failures, s.flush(&b, true, &failures)
}

func (s *shareSync) flush(b *batch, commit bool, failures *[]syncFailure) error {
	var r struct {
		Failures []syncFailure `json:"failures"`
	}
	ops := b.ops
	if ops == nil {
		ops = []map[string]any{}
	}
	*b = batch{}
	if err := s.guest("sync.apply", map[string]any{"ops": ops, "commit": commit}, &r); err != nil {
		return err
	}
	*failures = append(*failures, r.Failures...)
	s.progress(api.StageApplying)
	return nil
}

// failuresError names the files a sync could not change, grouped by why.
func failuresError(label, vm string, failures []syncFailure, r syncResult) error {
	msg := fmt.Sprintf("share sync of %s to VM %s failed for %d file(s)", label, vm, len(failures))
	groups := []struct {
		why   string
		match func(syncFailure) bool
	}{
		{"in use in the guest (stop the program holding them, then sync again)", func(f syncFailure) bool { return f.Locked && !f.Denied }},
		{"in use or access denied in the guest", func(f syncFailure) bool { return f.Locked && f.Denied }},
		{"access denied in the guest (read-only, or not writable by the Guest Service)", func(f syncFailure) bool { return !f.Locked && f.Denied }},
		{"failed", func(f syncFailure) bool { return !f.Locked && !f.Denied }},
	}
	for _, g := range groups {
		var some []string
		for _, f := range failures {
			if g.match(f) && len(some) < listLimit {
				some = append(some, fmt.Sprintf("%s (%s)", f.Path, f.Message))
			}
		}
		if len(some) > 0 {
			msg += "; " + g.why + ": " + strings.Join(some, ", ")
		}
	}
	msg += fmt.Sprintf(". The rest was synced: %d written, %d deleted, %d unchanged", r.written, r.deleted, r.unchanged)
	if len(r.overwritten) > 0 {
		msg += "; guest changes overwritten: " + strings.Join(r.overwritten, ", ")
	}
	return errors.New(msg)
}

// describe is the sync's summary, then the paths written (+), deleted (-),
// guest changes overwritten (!) and host paths skipped (~), as on macOS.
func (r syncResult) describe() string {
	lines := []string{fmt.Sprintf("%s -> %s: %d written, %d deleted, %d unchanged, %d bytes in %.1f s (hashed: %d on the host, %d in the guest)",
		r.share, r.guestPath, r.written, r.deleted, r.unchanged, r.bytes, r.seconds, r.hostHashed, r.guestHashed)}
	more := func(shown, total int) {
		if total > shown {
			lines = append(lines, fmt.Sprintf("  ... and %d more", total-shown))
		}
	}
	for _, p := range r.writtenPaths {
		lines = append(lines, "+ "+p)
	}
	more(len(r.writtenPaths), r.written)
	for _, p := range r.deletedPaths {
		lines = append(lines, "- "+p)
	}
	more(len(r.deletedPaths), r.deleted)
	if len(r.overwritten) > 0 {
		lines = append(lines, "changes made in the guest, overwritten:")
	}
	for _, p := range r.overwritten {
		lines = append(lines, "! "+p)
	}
	if len(r.skipped) > 0 {
		lines = append(lines, "not synced (links, junctions, special files, or files another program holds open):")
	}
	for _, p := range r.skipped {
		lines = append(lines, "~ "+p)
	}
	return strings.Join(lines, "\n")
}

// describeProgress is one line per running sync: the host's stage, then
// what the guest is on, as on macOS.
func describeProgress(p api.SyncProgress) string {
	mib := func(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
	if p.Stage == api.StageLive {
		lines := []string{fmt.Sprintf("%s: live, kept in step for %d s", p.Share, int(p.Seconds))}
		for _, c := range p.Conflicts {
			line := fmt.Sprintf("  ! %s: kept the %s's (%s)", c.Path, c.Winner, c.Why)
			if c.Kept != "" {
				line += fmt.Sprintf("; the other side's is branch %s", c.Kept)
			}
			lines = append(lines, line)
		}
		for _, l := range p.Locks {
			side, path, _ := strings.Cut(l, " ")
			lines = append(lines, fmt.Sprintf("  ! waiting for git: the %s holds %s; if no git runs there, it was left behind: delete it", side, path))
		}
		return strings.Join(lines, "\n")
	}
	line := fmt.Sprintf("%s: %s for %d s", p.Share, p.Stage, int(p.Seconds))
	if p.Stage == api.StageApplying {
		line += fmt.Sprintf(", sent %s of %s", mib(p.SentBytes), mib(p.TotalBytes))
	}
	if g := p.Guest; g != nil {
		verb, path := "writing", g.Path
		if g.Phase == "manifest" {
			verb = "hashing"
		}
		if path == "" {
			path = "(between files)"
		}
		line += fmt.Sprintf("; guest %s %s (%d files, %s in %d s)", verb, path, g.Files, mib(g.Bytes), int(g.Seconds))
	} else if p.Stage == api.StageGuestListing {
		line += "; guest progress unavailable (between calls)"
	}
	return line
}

// syncCacheDir is where glov keeps a VM's host hash caches:
// %LOCALAPPDATA%\glov\vms\<vm>\sync, which glov vm delete removes.
func syncCacheDir(vm string) (string, error) {
	f, err := sshFilesFor(vm)
	if err != nil {
		return "", err
	}
	return filepath.Join(f.dir, "sync"), nil
}

func runShareSync(ctx context.Context, c caller, vm, ref string, verify bool) error {
	msg, err := syncShare(ctx, c, vm, ref, verify)
	if err != nil {
		return err
	}
	fmt.Println(msg)
	return nil
}

// syncShare mirrors a share into the guest, or asks a live share's agent
// for a full round (glov share sync and the MCP share_sync).
func syncShare(ctx context.Context, c caller, vm, ref string, verify bool) (string, error) {
	if name, subdir, err := share.ParseRef(ref); err == nil {
		var list []api.ShareInfo
		if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: vm}, &list); err != nil {
			return "", err
		}
		if i := slices.IndexFunc(list, func(s api.ShareInfo) bool { return s.Name == name }); i >= 0 && list[i].Mode == share.Live {
			return reconcileLive(ctx, c, vm, name, subdir)
		}
	}
	s := &shareSync{c: c, ctx: ctx, vm: vm, cacheDir: syncCacheDir}
	r, err := s.run(ref, verify)
	if err != nil {
		return "", err
	}
	return r.describe(), nil
}

// reconcileLive asks a live share's agent for a full round, starting one
// when none runs.
func reconcileLive(ctx context.Context, c caller, vm, name, subdir string) (string, error) {
	if subdir != "" {
		return "", fmt.Errorf("share %s is live: it is kept in step whole, so sync it without /%s", name, subdir)
	}
	err := c.Call(ctx, api.MethodShareLiveReconcile, api.ShareParams{VM: vm, Name: name}, nil)
	if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeNotFound && strings.HasPrefix(pe.Message, "no glov share agent") {
		if err := startShareAgent(vm, name); err != nil {
			return "", err
		}
		return fmt.Sprintf("started the agent of live share %s; glov share status %s shows it once it holds the share", name, vm), nil
	}
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return "", errors.New(pe.Message)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("asked the agent of live share %s for a full round", name), nil
}

func runShareStatus(ctx context.Context, c caller, vm, name string, watch bool) error {
	for {
		var list []api.SyncProgress
		if err := c.Call(ctx, api.MethodShareStatus, api.ShareStatusParams{VM: vm, Name: name}, &list); err != nil {
			if pe, ok := errors.AsType[*proto.Error](err); ok {
				return errors.New(pe.Message)
			}
			return err
		}
		var shares []api.ShareInfo
		if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: vm}, &shares); err != nil {
			return err
		}
		idle := 0
		for _, s := range shares {
			if s.Mode == share.Live && (name == "" || s.Name == name) &&
				!slices.ContainsFunc(list, func(p api.SyncProgress) bool { return p.Share == s.Name }) {
				fmt.Printf("%s: live, but no agent keeps it in step (glov share sync %s %s starts one)\n", s.Name, vm, s.Name)
				idle++
			}
		}
		if len(list) == 0 {
			if idle == 0 {
				fmt.Println("no share sync running")
			}
			return nil
		}
		for _, p := range list {
			fmt.Println(describeProgress(p))
		}
		if !watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}
