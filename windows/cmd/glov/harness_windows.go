package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/share"
)

// signInTimeout bounds the wait for a harness VM's tty1 sign-in, which
// follows the Guest Service by a few seconds.
const signInTimeout = 2 * time.Minute

// projectShare is the share of a project's VM (glov claude, codex or
// agy), which is never reset, so its install freezes nothing.
const projectShare = "project"

// describeHarnesses is a line per secret a harness of the new VM needs:
// what it is, and how to define and grant it.
func describeHarnesses(ctx context.Context, c caller, vm string, names []string) string {
	var defined []string
	var list api.Secrets
	if len(names) > 0 && call(ctx, c, api.MethodSecretList, struct{}{}, &list) == nil {
		defined = []string{}
		for _, s := range list.Secrets {
			defined = append(defined, s.Name)
		}
	}
	var b strings.Builder
	for _, n := range names {
		if p, err := harness.Named(n); err == nil {
			for _, line := range p.SecretNeeds(vm, defined) {
				b.WriteString(line + "\n")
			}
		}
	}
	return b.String()
}

// runHarnessInstall installs the VM's harnesses (only those named in only,
// if any) with installCore. Then it stops the VM and freezes its disk as
// the template glov vm reset returns to; a VM frozen before is reset
// first, so the template takes only the install. A project's VM is
// installed where it runs instead, and keeps running.
func runHarnessInstall(ctx context.Context, c caller, vm string, only []string, noHostConfig bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil {
		return err
	}
	name := info.Name
	if !info.Isolated {
		return fmt.Errorf("VM %s is not isolated: a harness VM is created with glov vm create <vm> --harness <name>", name)
	}
	if len(info.Harnesses) == 0 {
		return fmt.Errorf("VM %s has no harness: it was created without --harness", name)
	}
	for _, h := range only {
		if !slices.Contains(info.Harnesses, h) {
			return fmt.Errorf("VM %s has no harness %s (it has %s); glov vm harness add %s %s adds it", name, h, strings.Join(info.Harnesses, ", "), name, h)
		}
	}
	names := info.Harnesses
	if len(only) > 0 {
		names = only
	}
	profiles, err := harness.Resolve(names)
	if err != nil {
		return err
	}
	var shares []api.ShareInfo
	if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: name}, &shares); err != nil {
		return err
	}
	// A project's VM (glov claude) keeps its work on its disk: no reset, no freeze.
	project := info.Project != nil || slices.ContainsFunc(shares, func(s api.ShareInfo) bool { return s.Name == projectShare })
	if info.Template && !project {
		if info.State == "running" {
			return fmt.Errorf("VM %s is running: glov vm stop %s first; the install starts it from its template", name, name)
		}
		if err := call(ctx, c, api.MethodVMReset, api.VMParams{VM: name}, &info); err != nil {
			return err
		}
		fmt.Printf("%s: reset to its template\n", name)
	}
	if info.State != "running" {
		if err := call(ctx, c, api.MethodVMStart, api.VMParams{VM: name}, &info); err != nil {
			return err
		}
		fmt.Printf("%s: started\n", name)
	}
	if err := call(ctx, c, api.MethodVMWait, api.WaitParams{VM: name, State: api.WaitGuestReady}, &info); err != nil {
		return err
	}
	if project {
		return installCore(ctx, c, name, profiles, shares, noHostConfig)
	}
	// The VM is frozen at the end, and a later glov vm guest update lasts
	// only until the next glov vm reset: freeze this glovd's build.
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: name}, &info); err != nil {
		return err
	}
	if g := info.GuestService; g != nil && g.Update != nil && g.Update.State == api.UpdateAvailable {
		var res api.GuestUpdated
		if err := call(ctx, c, api.MethodGuestUpdate, api.GuestUpdateParams{VM: name}, &res); err != nil {
			return err
		}
		fmt.Printf("%s: Guest Service updated from %s to %s\n", name, res.From, res.To)
	}
	if err := installCore(ctx, c, name, profiles, shares, noHostConfig); err != nil {
		return err
	}
	if err := call(ctx, c, api.MethodVMStop, api.StopParams{VM: name}, nil); err != nil {
		return fmt.Errorf("stop %s to freeze it: %w", name, err)
	}
	if err := call(ctx, c, api.MethodVMFreeze, api.VMParams{VM: name}, &info); err != nil {
		return err
	}
	var frozen api.Egress
	if err := call(ctx, c, api.MethodEgressList, api.EgressParams{VM: name}, &frozen); err != nil {
		return err
	}
	fmt.Printf("%s: stopped and frozen as its template (%s); glov vm reset returns to both\n", name, describeAllowlist(frozen))
	fmt.Print(strings.ReplaceAll(taskWorkflow, "<vm>", name))
	return nil
}

// installCore installs profiles in the running VM under their install
// allowlist and takes the added hosts away again, whatever happens: an
// error, Ctrl+C or a closed console window (SIGTERM). Only killing glov
// outright leaves them, which glov egress list shows. Then it copies the
// owner's harness config into the guest (unless noHostConfig) and links
// the conversation records to the harness-sessions share, if the VM has
// it.
func installCore(ctx context.Context, c caller, name string, profiles []harness.Profile, shares []api.ShareInfo, noHostConfig bool) error {
	// The install runs as the user the VM signs in on tty1 a few seconds
	// after the Guest Service is ready; wait for it before allowing hosts.
	if err := waitForUser(ctx, c, name); err != nil {
		return err
	}
	var before api.Egress
	if err := call(ctx, c, api.MethodEgressList, api.EgressParams{VM: name}, &before); err != nil {
		return err
	}
	var added []string
	for _, p := range profiles {
		for _, h := range p.Install {
			if !contains(before.Allow, h) && !contains(added, h) {
				added = append(added, h)
			}
		}
	}
	if len(added) > 0 {
		var e api.Egress
		if err := call(ctx, c, api.MethodEgressAllow, api.EgressParams{VM: name, Hosts: added}, &e); err != nil {
			return err
		}
		fmt.Printf("%s: allowed for the install: %s\n", name, strings.Join(added, ", "))
	}
	failure := installHarnesses(ctx, c, name, profiles)
	if len(added) > 0 {
		// Not ctx: an interrupted install must still take the hosts away.
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var e api.Egress
		if err := call(rctx, c, api.MethodEgressRemove, api.EgressParams{VM: name, Hosts: added}, &e); err != nil {
			also := ""
			if failure != nil {
				also = "; the install had failed too: " + failure.Error()
			}
			return fmt.Errorf("could not remove the install hosts (%v); run glov egress remove %s %s%s", err, name, strings.Join(added, " "), also)
		}
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "glov: interrupted; install hosts removed from %s\n", name)
		} else {
			fmt.Printf("%s: install hosts removed; %s\n", name, describeAllowlist(e))
		}
	}
	if failure != nil {
		if ctx.Err() != nil {
			return exitCode(130)
		}
		return failure
	}
	if noHostConfig {
		fmt.Printf("%s: host config not copied (--no-host-config)\n", name)
	} else if err := copyHostConfig(ctx, c, name, profiles); err != nil {
		return err
	}
	return linkSessions(ctx, c, name, profiles, shares)
}

// waitForUser waits up to signInTimeout for the guest user's tty1 sign-in.
func waitForUser(ctx context.Context, c caller, name string) error {
	deadline := time.Now().Add(signInTimeout)
	for waited := false; ; waited = true {
		_, err := runAsUser(ctx, c, name, []string{"true"}, nil)
		if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeNoUser {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nobody signed in on the console of %s within %v; a harness VM signs in by itself, so look at glov vm console %s", name, signInTimeout, name)
		}
		if !waited {
			fmt.Printf("%s: waiting for the console sign-in\n", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// copyHostConfig copies the profiles' HostConfig from the owner's profile
// folder into the guest user's home, replacing an earlier copy whole, so
// what was removed on the host goes from the guest too. It stages the
// copy, links already followed, in the scratch folder, the one host tree a
// copy into a VM may read besides its shares.
func copyHostConfig(ctx context.Context, c caller, vm string, profiles []harness.Profile) error {
	copies := harness.HostCopies(profiles)
	if len(copies) == 0 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	scratch, err := scratchDir()
	if err != nil {
		return err
	}
	staging, err := os.MkdirTemp(scratch, "host-config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	rep, err := harness.StageHostConfig(copies, home, staging)
	if err != nil {
		return err
	}
	guestHome := "/home/" + provision.User
	targets := []string{"rm", "-rf", "--"}
	for _, cp := range copies {
		targets = append(targets, guestHome+"/"+cp.Guest)
	}
	if code, out, err := guestRun(ctx, c, vm, targets); err != nil || code != 0 {
		return fmt.Errorf("could not remove the earlier host config copy in %s (exit code %d, %v): %s", vm, code, err, out)
	}
	var tops []string
	g := daemonGuest{ctx, c, vm}
	for _, cp := range rep.Copied {
		if _, err := toGuest(g, filepath.Join(staging, filepath.FromSlash(cp.Guest)), guestHome+"/"+cp.Guest); err != nil {
			return err
		}
		if top, _, _ := strings.Cut(cp.Guest, "/"); !slices.Contains(tops, guestHome+"/"+top) {
			tops = append(tops, guestHome+"/"+top)
		}
	}
	// The Guest Service writes as root; the copy and any folder it made
	// are the guest user's.
	if len(tops) > 0 {
		argv := append([]string{"chown", "-R", provision.User + ":" + provision.User, "--"}, tops...)
		if code, out, err := guestRun(ctx, c, vm, argv); err != nil || code != 0 {
			return fmt.Errorf("could not give the host config copy to %s in %s (exit code %d, %v): %s", provision.User, vm, code, err, out)
		}
	}
	for _, cp := range rep.Copied {
		fmt.Printf("%s: copied ~/%s (%d files, %s)\n", vm, cp.Guest, cp.Files, sizeText(cp.Bytes))
	}
	for _, s := range rep.Skipped {
		fmt.Printf("%s: left out %s\n", vm, s)
	}
	if len(rep.Copied) == 0 {
		fmt.Printf("%s: no host config to copy\n", vm)
	}
	return nil
}

// linkSessions makes each harness's record directories links into the
// harness-sessions share, or plain directories again on a VM without it.
func linkSessions(ctx context.Context, c caller, vm string, profiles []harness.Profile, shares []api.ShareInfo) error {
	i := slices.IndexFunc(shares, func(s api.ShareInfo) bool { return s.Name == harness.SessionsShare })
	if i < 0 {
		for _, p := range profiles {
			for _, dir := range p.Sessions {
				runAsUser(ctx, c, vm, []string{"/bin/sh", "-c", harness.SessionsUnlinkScript(dir)}, nil)
			}
		}
		return nil
	}
	if code, out, err := guestRun(ctx, c, vm, []string{"/bin/sh", "-c", harness.SessionsRootScript(provision.User)}); err != nil || code != 0 {
		return fmt.Errorf("could not create %s in %s (exit code %d, %v): %s", harness.SessionsGuestPath, vm, code, err, out)
	}
	for _, p := range profiles {
		for _, dir := range p.Sessions {
			if code, err := runAsUser(ctx, c, vm, []string{"/bin/sh", "-c", harness.SessionsLinkScript(p.Name, dir)}, os.Stdout); err != nil || code != 0 {
				return fmt.Errorf("could not link ~/%s to the conversation records share (exit code %d, %v)", dir, code, err)
			}
		}
	}
	fmt.Printf("%s: conversation records go to %s\n", vm, shares[i].HostPath)
	return nil
}

// runHarnessAdd sets a running isolated VM up for one more harness
// (vm.harnessAdd), then installs it as glov vm harness install --harness
// does, without freezing anything.
func runHarnessAdd(ctx context.Context, c caller, vm, name string, noHostConfig bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	prof, err := harness.Named(name)
	if err != nil {
		return err
	}
	fmt.Printf("%s: setting up %s\n", vm, prof.Title)
	var res api.HarnessAdded
	if err := call(ctx, c, api.MethodVMHarnessAdd, api.HarnessAddParams{VM: vm, Harness: prof.Name}, &res); err != nil {
		return err
	}
	if res.Already {
		fmt.Printf("%s already has %s\n", res.VM, prof.Name)
		return nil
	}
	for _, k := range res.Kept {
		fmt.Printf("%s: kept ~/%s, which is there already\n", res.VM, k)
	}
	line := res.VM + ": added " + prof.Name
	if len(res.Allowed) > 0 {
		line += ", allowed " + strings.Join(res.Allowed, ", ")
	}
	fmt.Println(line)
	fmt.Print(describeHarnesses(ctx, c, res.VM, []string{prof.Name}))
	if len(res.Withheld) > 0 {
		fmt.Printf("%s is open, so %s stays out of its tmpfs: any host could receive the real value; glov vm configure %s --egress open --allow-plain-secrets lets it in\n",
			res.VM, strings.Join(res.Withheld, ", "), res.VM)
	}
	var shares []api.ShareInfo
	if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: res.VM}, &shares); err != nil {
		return err
	}
	return installCore(ctx, c, res.VM, []harness.Profile{prof}, shares, noHostConfig)
}

// taskWorkflow is what to do with a frozen harness VM, printed after the
// install.
const taskWorkflow = `a task:
  glov share add <vm> <name> <fresh git clone --no-local> --ro    (once; shares stay across resets)
  glov vm start <vm> && glov vm wait <vm> guest-ready
  glov share sync <vm> <name>
  glov exec <vm> --as user --cwd <guest path> -- <harness command> ...
  glov exec <vm> --as user --cwd <guest path> -- git bundle create /tmp/task.bundle <base>..HEAD
  glov cp <vm>:/tmp/task.bundle %LOCALAPPDATA%\glov\scratch\task.bundle    (then git fetch it)
  glov vm stop <vm> && glov vm reset <vm>    (back to the template, allowlist included)
`

func installHarnesses(ctx context.Context, c caller, vm string, profiles []harness.Profile) error {
	for _, p := range profiles {
		fmt.Println("installing " + p.Title)
		code, err := runAsUser(ctx, c, vm, []string{"/bin/sh", "-c", p.InstallScript}, os.Stdout)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("installing %s failed with exit code %d", p.Name, code)
		}
		if code, err := runAsUser(ctx, c, vm, []string{p.Command, "--version"}, os.Stdout); err != nil || code != 0 {
			return fmt.Errorf("%s --version failed after the install (exit code %d, %v)", p.Command, code, err)
		}
	}
	return nil
}

// runAsUser runs argv as the signed-in user and returns its exit code; its
// output goes to w (nowhere if nil). An interrupted command is killed.
func runAsUser(ctx context.Context, c caller, vm string, argv []string, w interface{ Write([]byte) (int, error) }) (int, error) {
	var job api.JobStarted
	if err := c.Call(ctx, api.MethodExecStart, api.ExecParams{VM: vm, Argv: argv, As: "user"}, &job); err != nil {
		return 0, err
	}
	if w == nil {
		w = io.Discard
	}
	return follow(ctx, c, vm, job.JobID, w)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// recordsDir is the host folder of vm's conversation records share, if it
// has one; a failing share.list is no reason to fail the command around it.
func recordsDir(ctx context.Context, c caller, vm string) (string, bool) {
	var shares []api.ShareInfo
	if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: vm}, &shares); err != nil {
		return "", false
	}
	i := slices.IndexFunc(shares, func(s api.ShareInfo) bool { return s.Name == harness.SessionsShare })
	if i < 0 {
		return "", false
	}
	return shares[i].HostPath, true
}

// addSessionsShare gives a new harness VM its conversation records share:
// %LOCALAPPDATA%\glov\sessions\<vm>, live, at harness.SessionsGuestPath.
// The folder is outside the VM's own, so it outlives glov vm delete.
func addSessionsShare(ctx context.Context, c caller, vm string) error {
	local, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(local, "glov", harness.SessionsFolder, vm)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	host, err := share.Canonical(dir)
	if err != nil {
		return err
	}
	s := share.Share{Name: harness.SessionsShare, HostPath: host, Mode: share.Live, Guest: harness.SessionsGuestPath}
	if err := call(ctx, c, api.MethodShareAdd, api.ShareAddParams{VM: vm, Share: s}, nil); err != nil {
		return fmt.Errorf("the conversation records share: %w", err)
	}
	fmt.Printf("%s: conversation records are kept in %s\n", vm, host)
	return nil
}
