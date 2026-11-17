package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/share"
)

// runShare runs share add, remove and list. glov resolves and checks the
// host folder itself, as the user; glovd checks it again against its
// own directories and the VM's other shares, and records it.
func runShare(ctx context.Context, c *rpc.Client, cmd command) error {
	switch cmd.name {
	case "share add":
		s := cmd.share
		// Checked as written first, so a protected folder is refused for
		// what it is, even one the user cannot open or that is missing.
		abs, err := filepath.Abs(s.HostPath)
		if err != nil {
			return err
		}
		if _, err := share.Check(share.Share{Name: s.Name, HostPath: abs, Mode: s.Mode, Exclude: s.Exclude}, nil, share.HostProtected()); err != nil {
			return err
		}
		if s.HostPath, err = share.Canonical(s.HostPath); err != nil {
			return err
		}
		if s, err = share.Check(s, nil, share.HostProtected()); err != nil {
			return err
		}
		var info api.ShareInfo
		if err := call(ctx, c, api.MethodShareAdd, api.ShareAddParams{VM: cmd.vm, Share: s}, &info); err != nil {
			return err
		}
		fmt.Println("added share " + info.Describe())
		if s.Mode == share.Live {
			var vm api.VMInfo
			if call(ctx, c, api.MethodVMStatus, api.VMParams{VM: cmd.vm}, &vm) == nil && vm.State != "stopped" {
				if err := startShareAgent(vm.Name, s.Name); err != nil {
					return fmt.Errorf("share %s added, but not kept in step: %w", s.Name, err)
				}
			}
		}
	case "share remove":
		if err := call(ctx, c, api.MethodShareRemove, api.ShareParams{VM: cmd.vm, Name: cmd.share.Name}, nil); err != nil {
			return err
		}
		fmt.Println("removed share " + cmd.share.Name)
	case "share list":
		var list []api.ShareInfo
		if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: cmd.vm}, &list); err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no shares")
		}
		for _, s := range list {
			fmt.Println(s.Describe())
		}
	}
	return nil
}

// scratchDir is the folder every VM's copies may use besides its shares,
// as on macOS: %LOCALAPPDATA%\glov\scratch. It is created on first use.
func scratchDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "glov", "scratch")
	return dir, os.MkdirAll(dir, 0o700)
}

// allowCopy checks that a copy may read (write false) or write the host
// path: it must lie, after links are followed, in one of vm's shares (a rw
// one for a write) or in the scratch folder.
func allowCopy(ctx context.Context, c caller, vm, path string, write bool) error {
	var list []api.ShareInfo
	if err := call(ctx, c, api.MethodShareList, api.VMParams{VM: vm}, &list); err != nil {
		return err
	}
	shares := make([]share.Share, len(list))
	for i, s := range list {
		shares[i] = s.Share
	}
	scratch, err := scratchDir()
	if err != nil {
		return err
	}
	resolved, err := share.Resolve(path)
	if err != nil {
		return err
	}
	extra := []string{scratch}
	if s, err := share.Resolve(scratch); err == nil {
		extra = append(extra, s)
	}
	if share.Allowed(resolved, shares, extra, write) {
		return nil
	}
	what := "one of its shares"
	if write {
		what = "one of its rw shares"
	}
	return fmt.Errorf("host path %s is outside VM %s's copy allowlist: put it in %s or in %s (glov share list %s)", path, vm, what, scratch, vm)
}
