package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/store"
)

// harnessAddTimeout bounds the guest commands of vm.harnessAdd.
const harnessAddTimeout = time.Minute

// writeNewScript runs as the guest user in its home with path and base64
// content pairs: writes each file (mode 0600) unless something is there,
// which it keeps and names ("kept <path>").
const writeNewScript = `cd "$HOME" || exit 1
while [ $# -ge 2 ]; do
  p=$1; c=$2; shift 2
  if [ -e "$p" ] || [ -L "$p" ]; then echo "kept $p"; continue; fi
  mkdir -p "$(dirname "$p")" || exit 1
  (umask 077; printf '%s' "$c" | base64 -d > "$p") || exit 1
done
`

// linkScript runs as the guest user in its home with path and target
// pairs, as provisioning links a tmpfs secret's file.
const linkScript = `cd "$HOME" || exit 1
while [ $# -ge 2 ]; do mkdir -p "$(dirname "$1")" && ln -sfn "$2" "$1" || exit 1; shift 2; done
`

// harnessAdd sets the running guest up for one more harness: its
// environment in /etc/environment, its home files (one already there is
// kept), its secret links and, if it needs them, user namespaces; then
// allows its runtime hosts and records it. A VM created without --harness
// is refused before anything runs in it, and so is a frozen one: glov vm
// reset would bring back the disk without it.
func (d *Daemon) harnessAdd(caller string, p api.HarnessAddParams) (api.HarnessAdded, error) {
	r, unlock, err := d.lockedIsolated(caller, p.VM)
	if err != nil {
		return api.HarnessAdded{}, err
	}
	defer unlock()
	prof, err := harness.Named(p.Harness)
	if err != nil {
		return api.HarnessAdded{}, proto.Errorf(proto.CodeInvalidParams, "%v", err)
	}
	res := api.HarnessAdded{VM: r.Name, Harness: prof.Name}
	if slices.Contains(r.Harnesses, prof.Name) {
		res.Already = true
		return res, nil
	}
	if len(r.Harnesses) == 0 {
		// Only vm create --harness makes the guest user glov a harness runs as.
		return res, fmt.Errorf("VM %s was created without --harness, which glov vm harness add cannot make up for: create a harness VM with glov vm create <vm> --isolated --harness %s", r.Name, prof.Name)
	}
	if r.Template != nil {
		return res, fmt.Errorf("VM %s is frozen as a template, which glov vm reset would bring back without %s: add harnesses before glov vm harness install", r.Name, prof.Name)
	}
	if r.ID == "" || !d.VMs.Running(r.ID) {
		return res, fmt.Errorf("VM %s is not running: glov vm start %s, then glov vm wait %s guest-ready", r.Name, r.Name, r.Name)
	}
	if _, gs := d.VMs.Info(r.ID); gs == nil || gs.State != "ready" {
		return res, fmt.Errorf("the Guest Service of VM %s is not ready: glov vm wait %s guest-ready", r.Name, r.Name)
	}
	profiles, err := harness.Resolve(append(slices.Clone(r.Harnesses), prof.Name))
	if err != nil {
		return res, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), harnessAddTimeout)
	defer cancel()
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	run := func(what string, argv []string, line func(string)) error {
		var out []string
		code, err := d.runGuest(ctx, r, argv, func(l string) {
			out = append(out, l)
			if line != nil {
				line(l)
			}
		})
		if err == nil && code != 0 {
			err = fmt.Errorf("%s exited %d: %s", what, code, strings.Join(out[max(0, len(out)-3):], "; "))
		}
		return err
	}
	asUser := func(script string, args ...string) []string {
		return append([]string{"sh", "-c", `u=$1; shift; exec runuser -u "$u" -- sh -c "$@"`, "sh", provision.User, script, "sh"}, args...)
	}

	env, err := d.readGuestFile(ctx, r, "/etc/environment")
	if err != nil {
		return res, err
	}
	var subscribed []string
	if r.Egress != nil {
		subscribed = r.Egress.Subscriptions
	}
	system := []string{"sh", "-c", putScript, "sh"}
	if wanted := harness.Environment(env, profiles, subscribed, r.Name); wanted != env {
		system = append(system, "/etc/environment", "644", b64(wanted))
	}
	if prof.UserNamespaces {
		system = append(system, provision.UserNamespacesPath, "644", b64("kernel.apparmor_restrict_unprivileged_userns = 0\n"))
	}
	if len(system) > 4 {
		if err := run("writing the harness environment", system, nil); err != nil {
			return res, err
		}
	}
	// -e: Debian has no AppArmor, so neither the key nor a restriction to lift.
	if prof.UserNamespaces {
		if err := run("applying "+provision.UserNamespacesPath, []string{"sysctl", "-e", "-p", provision.UserNamespacesPath}, nil); err != nil {
			return res, err
		}
	}
	var files []string
	for _, f := range prof.GuestFiles() {
		files = append(files, f.Path, b64(f.Content()))
	}
	if len(files) > 0 {
		err := run("writing the harness files", asUser(writeNewScript, files...), func(l string) {
			if kept, ok := strings.CutPrefix(l, "kept "); ok {
				res.Kept = append(res.Kept, kept)
			}
		})
		if err != nil {
			return res, err
		}
	}
	var links []string
	for _, l := range prof.SecretLinks() {
		links = append(links, l.Path, l.Target)
	}
	if len(links) > 0 {
		if err := run("linking the harness secrets", asUser(linkScript, links...), nil); err != nil {
			return res, err
		}
	}

	e := store.Egress{Allow: []string{}}
	if r.Egress != nil {
		e = *r.Egress
	}
	for _, h := range prof.Runtime {
		if !slices.Contains(e.Allow, h) {
			res.Allowed = append(res.Allowed, h)
		}
	}
	if e.Allow, err = egress.Allowing(e.Allow, res.Allowed); err != nil {
		return res, err
	}
	r.Egress = &e
	r.Harnesses = append(slices.Clone(r.Harnesses), prof.Name)
	if err := d.Store.Save(r); err != nil {
		return res, err
	}
	if e.Open {
		for _, s := range prof.Secrets {
			if s.Delivery == harness.Tmpfs && slices.Contains(e.Secrets, s.Name) && !slices.Contains(e.PlainSecrets, s.Name) {
				res.Withheld = append(res.Withheld, s.Name)
			}
		}
	}
	slog.Info("harness added", "vm", r.Name, "harness", prof.Name, "allowed", res.Allowed)
	// Secrets already granted reach the new harness's tmpfs files now.
	owner, name := r.Owner, r.Name
	go d.syncSecrets(owner, name)
	return res, nil
}
