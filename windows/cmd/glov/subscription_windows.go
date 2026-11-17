package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/rpc"
)

// runSubscription runs glov secret login and logout.
func runSubscription(ctx context.Context, c *rpc.Client, cmd command) error {
	name := cmd.names[0]
	if cmd.name == "secret logout" {
		return logout(ctx, c, cmd.vm, name)
	}
	sub, err := subscriptionOf(name)
	if err != nil {
		return err
	}
	p := api.SecretLoginParams{VM: cmd.vm, Harness: name}
	if sub.SignIn == nil {
		if sub.PasteHint != "" {
			fmt.Fprintln(os.Stderr, sub.PasteHint)
		}
		if p.Value, err = readValue(os.Stdin, "Token for "+name+": "); err != nil {
			return err
		}
		if p.Value == "" {
			return errors.New("no token was pasted")
		}
	}
	var r api.SecretLogin
	if err := call(ctx, c, api.MethodSecretLogin, p, &r); err != nil {
		return err
	}
	if len(r.Command) == 0 {
		fmt.Printf("%s: %s signed in to the subscription\n", r.VM, name)
		return nil
	}
	if !isConsole(os.Stdin) {
		fmt.Printf("%s: %s switched to the subscription; sign in with glov ssh %s -- -t %s\n", r.VM, name, r.VM, shellQuote(r.Command))
		return nil
	}
	code, err := runInGuest(ctx, c, r.VM, r.Command, true)
	if err != nil {
		return err
	}
	var grants api.SecretGrants
	if err := call(ctx, c, api.MethodSecretGrants, api.SecretGrantParams{VM: r.VM}, &grants); err != nil {
		return err
	}
	if missing := stillMissing(r, grants); len(missing) > 0 {
		how := "ended"
		if code != 0 {
			how = fmt.Sprintf("failed (exit %d)", code)
		}
		return fmt.Errorf("the sign-in %s without storing %s; run glov secret login %s %s again, or glov secret logout %s %s to go back to the API key",
			how, strings.Join(missing, ", "), r.VM, name, r.VM, name)
	}
	fmt.Printf("%s: %s signed in to the subscription\n", r.VM, name)
	return nil
}

func logout(ctx context.Context, c *rpc.Client, vm, name string) error {
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil {
		return err
	}
	if sub, err := subscriptionOf(name); err == nil && len(sub.SignOut) > 0 && info.State == "running" {
		var grants api.SecretGrants
		if err := call(ctx, c, api.MethodSecretGrants, api.SecretGrantParams{VM: info.Name}, &grants); err != nil {
			return err
		}
		if slices.ContainsFunc(grants.Names, func(n string) bool { return strings.HasPrefix(n, info.Name+"."+name+".") }) {
			code, err := runInGuest(ctx, c, info.Name, sub.SignOut, false)
			if err != nil {
				return err
			}
			if code != 0 {
				fmt.Fprintf(os.Stderr, "glov: %s in the VM failed (exit %d); signing out on the host anyway\n", strings.Join(sub.SignOut, " "), code)
			}
		}
	}
	var r api.SecretLogin
	if err := call(ctx, c, api.MethodSecretLogout, api.SecretLoginParams{VM: info.Name, Harness: name}, &r); err != nil {
		return err
	}
	fmt.Printf("%s: %s signed out of the subscription; it uses its API key again\n", r.VM, name)
	return nil
}

// runInGuest runs argv in the VM as user glov over SSH in a login shell,
// in this terminal when terminal is set, and returns its exit code.
func runInGuest(ctx context.Context, c *rpc.Client, vm string, argv []string, terminal bool) (int, error) {
	var extra []string
	if terminal {
		extra = append(extra, "-t")
	}
	err := runSSH(ctx, c, vm, append(extra, guestLoginShell(argv)))
	if e, ok := errors.AsType[exitCode](err); ok {
		return int(e), nil
	}
	return 0, err
}
