package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/store"
)

// modeSyncTimeout bounds one switch of a guest's harness sign-ins.
const modeSyncTimeout = 60 * time.Second

// tokenEndpoints are the token endpoints of VM vm's subscribed harnesses,
// with the secrets their answers are kept as.
func tokenEndpoints(vm string, subscriptions []string) []egress.TokenEndpoint {
	var out []egress.TokenEndpoint
	for _, p := range harness.All {
		sub := p.Subscription
		if sub == nil || !slices.Contains(subscriptions, p.Name) {
			continue
		}
		for _, e := range sub.TokenEndpoints {
			answer := map[string]egress.KeptSecret{}
			for field, secret := range e.Answer {
				var hosts []string
				if i := slices.IndexFunc(sub.Secrets, func(s harness.Secret) bool { return s.Name == secret }); i >= 0 {
					hosts = sub.Secrets[i].Hosts
				}
				answer[field] = egress.KeptSecret{Name: p.SecretName(vm, secret), Hosts: hosts}
			}
			out = append(out, egress.TokenEndpoint{Host: e.Host, Path: e.Path, Answer: answer})
		}
	}
	return out
}

// subscriptionProfile is the profile of harness, one isolated VM ref was
// created with that has a subscription sign-in.
func (d *Daemon) subscriptionProfile(caller, ref, name string) (store.Record, harness.Profile, error) {
	r, err := d.isolated(caller, ref)
	if err != nil {
		return store.Record{}, harness.Profile{}, err
	}
	if !slices.Contains(r.Harnesses, name) {
		has := strings.Join(r.Harnesses, ", ")
		if has == "" {
			has = "none"
		}
		return store.Record{}, harness.Profile{}, fmt.Errorf("VM %s has no harness %s: its harnesses are %s", r.Name, name, has)
	}
	p, err := namedProfile(name)
	if err != nil {
		return store.Record{}, harness.Profile{}, err
	}
	if p.Subscription == nil {
		return store.Record{}, harness.Profile{}, fmt.Errorf("harness %s has no subscription sign-in", name)
	}
	return r, p, nil
}

// subscriptionNames are the VM's names of p's subscription secrets.
func subscriptionNames(vm string, p harness.Profile) []string {
	var out []string
	for _, s := range p.Subscription.Secrets {
		out = append(out, p.SecretName(vm, s.Name))
	}
	return out
}

// secretLogin signs VM p.VM's harness in to the owner's subscription
// (docs/networking.md "Subscriptions"): its tokens become secrets
// <vm>.<harness>.<name>, granted to the VM; its runtime hosts are allowed;
// and the guest is switched to the subscription now if it runs, else at its
// next Guest Service session.
func (d *Daemon) secretLogin(caller string, p api.SecretLoginParams) (api.SecretLogin, error) {
	if d.Secrets == nil {
		return api.SecretLogin{}, errNoSecrets
	}
	r, prof, err := d.subscriptionProfile(caller, p.VM, p.Harness)
	if err != nil {
		return api.SecretLogin{}, err
	}
	sub := prof.Subscription
	names := subscriptionNames(r.Name, prof)
	if sub.SignIn == nil {
		hint := sub.PasteHint
		if hint == "" {
			hint = "see the harness's docs"
		}
		if p.Value == "" {
			return api.SecretLogin{}, proto.Errorf(proto.CodeInvalidParams, "value: paste the token (%s)", hint)
		}
		s := sub.Secrets[0]
		if _, err := d.Secrets.Set(prof.SecretName(r.Name, s.Name), s.Hosts, p.Value, nil, ""); err != nil {
			return api.SecretLogin{}, err
		}
	}
	if _, err := d.editEgress(caller, r.Name, func(_ string, e *store.Egress) error {
		if !slices.Contains(e.Subscriptions, prof.Name) {
			e.Subscriptions = append(e.Subscriptions, prof.Name)
		}
		for _, h := range sub.Runtime {
			if !slices.Contains(e.Allow, h) {
				e.Allow = append(e.Allow, h)
			}
		}
		for _, n := range names {
			if !slices.Contains(e.Secrets, n) {
				e.Secrets = append(e.Secrets, n)
			}
		}
		return nil
	}); err != nil {
		return api.SecretLogin{}, err
	}
	slog.Info("subscription signed in", "vm", r.Name, "harness", prof.Name)
	if err := d.resyncSecrets(caller, []string{r.Name}); err != nil {
		return api.SecretLogin{}, err
	}
	if err := d.syncHarnessMode(r.Owner, r.Name, true); err != nil {
		return api.SecretLogin{}, fmt.Errorf("signed in on the host, but %v; the next Guest Service session retries", err)
	}
	defined, err := d.definedNames()
	if err != nil {
		return api.SecretLogin{}, err
	}
	res := api.SecretLogin{VM: r.Name, Harness: prof.Name, Command: sub.SignIn, Missing: []string{}}
	for _, n := range names {
		if !slices.Contains(defined, n) {
			res.Missing = append(res.Missing, n)
		}
	}
	return res, nil
}

// secretLogout undoes secretLogin: removes the harness's subscription
// secrets and their grants, the runtime hosts no other harness of the VM
// needs, and switches the guest back to the API key. The caller runs the
// profile's signOut in the guest first, while the proxy can still put the
// real token into a revocation.
func (d *Daemon) secretLogout(caller string, p api.SecretLoginParams) (api.SecretLogin, error) {
	if d.Secrets == nil {
		return api.SecretLogin{}, errNoSecrets
	}
	r, prof, err := d.subscriptionProfile(caller, p.VM, p.Harness)
	if err != nil {
		return api.SecretLogin{}, err
	}
	if r.Egress == nil || !slices.Contains(r.Egress.Subscriptions, prof.Name) {
		return api.SecretLogin{}, fmt.Errorf("harness %s of VM %s is not signed in to a subscription: glov secret login %s %s", prof.Name, r.Name, r.Name, prof.Name)
	}
	sub := prof.Subscription
	names := subscriptionNames(r.Name, prof)
	needed := slices.Clone(prof.Runtime)
	for _, h := range r.Harnesses {
		if h == prof.Name {
			continue
		}
		other, err := namedProfile(h)
		if err != nil {
			continue
		}
		needed = append(needed, other.Runtime...)
		if other.Subscription != nil && slices.Contains(r.Egress.Subscriptions, h) {
			needed = append(needed, other.Subscription.Runtime...)
		}
	}
	if _, err := d.editEgress(caller, r.Name, func(_ string, e *store.Egress) error {
		e.Subscriptions = slices.DeleteFunc(e.Subscriptions, func(h string) bool { return h == prof.Name })
		if len(e.Subscriptions) == 0 {
			e.Subscriptions = nil
		}
		e.Allow = slices.DeleteFunc(e.Allow, func(h string) bool { return slices.Contains(sub.Runtime, h) && !slices.Contains(needed, h) })
		e.Secrets = slices.DeleteFunc(e.Secrets, func(n string) bool { return slices.Contains(names, n) })
		if len(e.Secrets) == 0 {
			e.Secrets = nil
		}
		return nil
	}); err != nil {
		return api.SecretLogin{}, err
	}
	for _, n := range names {
		if _, err := d.Secrets.Remove(n); err != nil {
			return api.SecretLogin{}, err
		}
	}
	slog.Info("subscription signed out", "vm", r.Name, "harness", prof.Name)
	if err := d.resyncSecrets(caller, []string{r.Name}); err != nil {
		return api.SecretLogin{}, err
	}
	if err := d.syncHarnessMode(r.Owner, r.Name, true); err != nil {
		return api.SecretLogin{}, fmt.Errorf("signed out on the host, but %v; the next Guest Service session retries", err)
	}
	return api.SecretLogin{VM: r.Name, Harness: prof.Name, Missing: []string{}}, nil
}

// syncHarnessMode points each harness in the running VM's guest at the
// subscription or at the API key, as vm.json says: the keys of
// /etc/environment and the settings both modes set, then the marker.
// Without force it does nothing when the marker already says so. One at a
// time per VM; a stopped VM or one without harnesses is left alone.
func (d *Daemon) syncHarnessMode(owner, name string, force bool) error {
	m, _ := d.modeSyncs.LoadOrStore(name, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	defer m.(*sync.Mutex).Unlock()
	r, err := d.Store.Load(owner, name)
	if err != nil || !r.Isolated || r.ID == "" || len(r.Harnesses) == 0 || !d.VMs.Running(r.ID) {
		return nil
	}
	profiles, err := harness.Resolve(r.Harnesses)
	if err != nil {
		return nil
	}
	var subscribed []string
	if r.Egress != nil {
		subscribed = slices.Clone(r.Egress.Subscriptions)
	}
	slices.Sort(subscribed)
	ctx, cancel := context.WithTimeout(context.Background(), modeSyncTimeout)
	defer cancel()
	changed, err := d.applyHarnessMode(ctx, r, profiles, subscribed, force)
	if err != nil {
		err = fmt.Errorf("cannot switch the harness sign-in: %v", err)
		slog.Warn(err.Error(), "vm", name)
		return err
	}
	if changed {
		on := strings.Join(subscribed, ", ")
		if on == "" {
			on = "none"
		}
		slog.Info("harnesses on subscriptions now "+on, "vm", name)
	}
	return nil
}

// applyHarnessMode does syncHarnessMode's work and reports whether it
// switched the guest.
func (d *Daemon) applyHarnessMode(ctx context.Context, r store.Record, profiles []harness.Profile, subscribed []string, force bool) (bool, error) {
	marker, err := d.readGuestFile(ctx, r, harness.MarkerPath)
	if err != nil {
		return false, err
	}
	if !force && slices.Equal(harness.ParseMarker(marker), subscribed) {
		return false, nil
	}
	env, err := d.readGuestFile(ctx, r, "/etc/environment")
	if err != nil {
		return false, err
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	run := func(what string, argv []string) error {
		var out []string
		code, err := d.runGuest(ctx, r, argv, func(line string) { out = append(out, line) })
		if err == nil && code != 0 {
			err = fmt.Errorf("%s exited %d: %s", what, code, strings.Join(out[max(0, len(out)-3):], "; "))
		}
		return err
	}
	edits := harness.Edits(profiles, subscribed, r.Name)
	var paths []string
	for _, e := range edits {
		if !slices.Contains(paths, e.Path) {
			paths = append(paths, e.Path)
		}
	}
	// The guest user's files, as that user, so its home stays its own.
	user := []string{"sh", "-c", `u=$1; shift; exec runuser -u "$u" -- sh -c "$@"`, "sh", provision.User, userPutScript, "sh"}
	for _, p := range paths {
		text, err := d.readGuestFile(ctx, r, path.Join("/home", provision.User, p))
		if err != nil {
			return false, err
		}
		edited, ok := harness.Apply(edits, text, p)
		if !ok {
			return false, fmt.Errorf("~/%s is not valid JSON; fix or remove it, then glov secret login again", p)
		}
		if edited != text {
			user = append(user, p, "600", b64(edited))
		}
	}
	if len(user) > 8 {
		if err := run("writing the harness settings", user); err != nil {
			return false, err
		}
	}
	system := []string{"sh", "-c", putScript, "sh"}
	if wanted := harness.Environment(env, profiles, subscribed, r.Name); wanted != env {
		system = append(system, "/etc/environment", "644", b64(wanted))
	}
	// The marker last, so a switch cut short is done again.
	system = append(system, harness.MarkerPath, "644", b64(harness.Marker(subscribed)))
	if err := run("writing the harness environment", system); err != nil {
		return false, err
	}
	return true, nil
}
