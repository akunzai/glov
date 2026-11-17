package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/secret"
	"github.com/akunzai/glov/windows/internal/store"
)

// errNoSecrets reports a glovd without a secret store, as tests build it.
var errNoSecrets = errors.New("glovd has no secret store")

// egressTLS is the TLS interception every isolated VM's proxy shares, its
// CA made in CADir on first use.
func (d *Daemon) egressTLS() (*egress.TLS, error) {
	d.tlsMu.Lock()
	defer d.tlsMu.Unlock()
	if d.tls != nil {
		return d.tls, nil
	}
	if d.CADir == "" {
		return nil, errors.New("glovd has no egress CA directory")
	}
	ca, err := egress.LoadOrCreateCA(d.CADir)
	if err != nil {
		return nil, err
	}
	d.tls = &egress.TLS{CA: ca}
	return d.tls, nil
}

func summary(def secret.Definition) api.Secret {
	return api.Secret{Name: def.Name, Source: def.Source, Command: def.Command, Hosts: def.Hosts, Length: def.Length, Ending: def.Ending, Forge: string(def.Forge)}
}

func (d *Daemon) secretSet(caller string, p api.SecretParams, source bool) (api.Secret, error) {
	if p.Name == "" {
		return api.Secret{}, proto.Errorf(proto.CodeInvalidParams, "name: name the secret")
	}
	if d.Secrets == nil {
		return api.Secret{}, errNoSecrets
	}
	var command []string
	if source {
		if len(p.Command) == 0 {
			return api.Secret{}, proto.Errorf(proto.CodeInvalidParams, "command: give the command that prints the secret")
		}
		command = p.Command
	}
	forge := egress.Forge(p.Forge)
	if forge != "" && !slices.Contains(egress.Forges, forge) {
		return api.Secret{}, proto.Errorf(proto.CodeInvalidParams, "forge %s: use github or gitlab", p.Forge)
	}
	def, err := d.Secrets.Set(p.Name, p.Hosts, p.Value, command, forge)
	if err != nil {
		return api.Secret{}, err
	}
	slog.Info("secret stored", "name", def.Name, "source", def.Source, "hosts", def.Hosts)
	return summary(def), d.resyncSecrets(caller, d.holders(caller, def.Name))
}

func (d *Daemon) secretRefresh(caller string, p api.SecretParams) (api.Secret, error) {
	if p.Name == "" {
		return api.Secret{}, proto.Errorf(proto.CodeInvalidParams, "name: name the secret")
	}
	if d.Secrets == nil {
		return api.Secret{}, errNoSecrets
	}
	def, err := d.Secrets.Refresh(p.Name, p.Value)
	if errors.Is(err, secret.ErrNotFound) {
		return api.Secret{}, proto.Errorf(proto.CodeNotFound, "no secret named %s", p.Name)
	}
	if err != nil {
		return api.Secret{}, err
	}
	slog.Info("secret refreshed", "name", def.Name)
	return summary(def), d.resyncSecrets(caller, d.holders(caller, def.Name))
}

func (d *Daemon) secretList() (api.Secrets, error) {
	if d.Secrets == nil {
		return api.Secrets{}, errNoSecrets
	}
	all, err := d.Secrets.List()
	res := api.Secrets{Secrets: []api.Secret{}}
	for _, def := range all {
		res.Secrets = append(res.Secrets, summary(def))
	}
	return res, err
}

// secretRemove deletes the secret and takes it from every VM granted it.
func (d *Daemon) secretRemove(caller string, p api.SecretParams) error {
	if p.Name == "" {
		return proto.Errorf(proto.CodeInvalidParams, "name: name the secret")
	}
	if d.Secrets == nil {
		return errNoSecrets
	}
	ok, err := d.Secrets.Remove(p.Name)
	if err != nil {
		return err
	}
	if !ok {
		return proto.Errorf(proto.CodeNotFound, "no secret named %s", p.Name)
	}
	slog.Info("secret removed", "name", p.Name)
	holders := d.holders(caller, p.Name)
	for _, vm := range holders {
		if _, err := d.editEgress(caller, vm, func(_ string, e *store.Egress) error {
			e.Secrets = slices.DeleteFunc(e.Secrets, func(n string) bool { return n == p.Name })
			if len(e.Secrets) == 0 {
				e.Secrets = nil
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return d.resyncSecrets(caller, holders)
}

func (d *Daemon) secretGrant(caller string, p api.SecretGrantParams, grant bool) (api.SecretGrants, error) {
	if len(p.Names) == 0 {
		return api.SecretGrants{}, proto.Errorf(proto.CodeInvalidParams, "names: name at least one secret")
	}
	if grant {
		defined, err := d.definedNames()
		if err != nil {
			return api.SecretGrants{}, err
		}
		if unknown := slices.DeleteFunc(slices.Clone(p.Names), func(n string) bool { return slices.Contains(defined, n) }); len(unknown) > 0 {
			return api.SecretGrants{}, fmt.Errorf("no secret named %s: define it with glov secret set or glov secret source", strings.Join(unknown, ", "))
		}
	}
	e, err := d.editEgress(caller, p.VM, func(name string, e *store.Egress) error {
		if grant {
			if e.Open {
				plain, err := d.tmpfsAmong(caller, name, p.Names)
				if err != nil {
					return err
				}
				if len(plain) > 0 && !p.AllowPlainSecrets {
					return plainSecretsRefusal(name, plain, "glov secret grant "+name+" "+strings.Join(p.Names, " ")+" --allow-plain-secrets")
				}
				for _, n := range plain {
					if !slices.Contains(e.PlainSecrets, n) {
						e.PlainSecrets = append(e.PlainSecrets, n)
					}
				}
			}
			for _, n := range p.Names {
				if !slices.Contains(e.Secrets, n) {
					e.Secrets = append(e.Secrets, n)
				}
			}
			return nil
		}
		if missing := slices.DeleteFunc(slices.Clone(p.Names), func(n string) bool { return slices.Contains(e.Secrets, n) }); len(missing) > 0 {
			return fmt.Errorf("VM %s was not granted %s", name, strings.Join(missing, ", "))
		}
		e.Secrets = slices.DeleteFunc(e.Secrets, func(n string) bool { return slices.Contains(p.Names, n) })
		if len(e.Secrets) == 0 {
			e.Secrets = nil
		}
		return nil
	})
	if err != nil {
		return api.SecretGrants{}, err
	}
	if err := d.resyncSecrets(caller, []string{e.VM}); err != nil {
		return api.SecretGrants{}, err
	}
	return d.secretGrants(caller, api.SecretGrantParams{VM: e.VM})
}

func (d *Daemon) secretGrants(caller string, p api.SecretGrantParams) (api.SecretGrants, error) {
	r, err := d.isolated(caller, p.VM)
	if err != nil {
		return api.SecretGrants{}, err
	}
	res := api.SecretGrants{VM: r.Name, Names: []string{}, Missing: []string{}}
	if r.Egress != nil && len(r.Egress.Secrets) > 0 {
		res.Names = r.Egress.Secrets
		defined, err := d.definedNames()
		if err != nil {
			return api.SecretGrants{}, err
		}
		for _, n := range res.Names {
			if !slices.Contains(defined, n) {
				res.Missing = append(res.Missing, n)
			}
		}
	}
	return res, nil
}

// secretRepos changes and answers the repositories an isolated VM may
// reach with its git secrets.
func (d *Daemon) secretRepos(caller string, p api.SecretReposParams) (api.SecretRepos, error) {
	patterns := func(texts []string) ([]string, error) {
		var out []string
		for _, text := range texts {
			pattern, ok := egress.ParseRepoPattern(text)
			if !ok {
				return nil, proto.Errorf(proto.CodeInvalidParams, "repository %s: give <host>/<owner>/<repo> or <host>/<group>/*", text)
			}
			out = append(out, pattern.String())
		}
		return out, nil
	}
	add, err := patterns(p.Add)
	if err != nil {
		return api.SecretRepos{}, err
	}
	remove, err := patterns(p.Remove)
	if err != nil {
		return api.SecretRepos{}, err
	}
	reposOf := func(name string, e *store.Egress) api.SecretRepos {
		res := api.SecretRepos{VM: name, Repos: []string{}}
		if e != nil && len(e.Repos) > 0 {
			res.Repos = e.Repos
		}
		return res
	}
	if len(add) == 0 && len(remove) == 0 {
		r, err := d.isolated(caller, p.VM)
		if err != nil {
			return api.SecretRepos{}, err
		}
		return reposOf(r.Name, r.Egress), nil
	}
	var res api.SecretRepos
	_, err = d.editEgress(caller, p.VM, func(name string, e *store.Egress) error {
		if missing := slices.DeleteFunc(slices.Clone(remove), func(r string) bool { return slices.Contains(e.Repos, r) }); len(missing) > 0 {
			return fmt.Errorf("VM %s has no repository %s", name, strings.Join(missing, ", "))
		}
		e.Repos = slices.DeleteFunc(e.Repos, func(r string) bool { return slices.Contains(remove, r) })
		for _, r := range add {
			if !slices.Contains(e.Repos, r) {
				e.Repos = append(e.Repos, r)
			}
		}
		if len(e.Repos) == 0 {
			e.Repos = nil
		}
		res = reposOf(name, e)
		return nil
	})
	return res, err
}

func (d *Daemon) definedNames() ([]string, error) {
	if d.Secrets == nil {
		return nil, errNoSecrets
	}
	all, err := d.Secrets.List()
	var names []string
	for _, def := range all {
		names = append(names, def.Name)
	}
	return names, err
}

// holders are caller's VMs granted the secret called name.
func (d *Daemon) holders(caller, name string) []string {
	names, _ := d.Store.Names(caller)
	var out []string
	for _, vm := range names {
		if r, err := d.Store.Load(caller, vm); err == nil && r.Egress != nil && slices.Contains(r.Egress.Secrets, name) {
			out = append(out, vm)
		}
	}
	return out
}

// namedProfile is harness.Named, which tests replace.
var namedProfile = harness.Named

// tmpfsDeclared are the secrets r's harnesses take as real values in tmpfs.
func tmpfsDeclared(r store.Record) []string {
	var out []string
	for _, h := range r.Harnesses {
		p, err := namedProfile(h)
		if err != nil {
			continue
		}
		for _, s := range p.Secrets {
			if s.Delivery == harness.Tmpfs && !slices.Contains(out, s.Name) {
				out = append(out, s.Name)
			}
		}
	}
	return out
}

// tmpfsAmong are those of names that caller's VM name's harnesses take as
// real values in tmpfs.
func (d *Daemon) tmpfsAmong(caller, name string, names []string) ([]string, error) {
	r, err := d.Store.Load(caller, name)
	if err != nil {
		return nil, err
	}
	declared := tmpfsDeclared(r)
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !slices.Contains(declared, n) }), nil
}

// syncTimeout bounds one secrets.sync.
const syncTimeout = 30 * time.Second

// guestSecret is one secrets.sync entry; Data is base64 on the wire.
type guestSecret struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// syncSecrets makes the running VM's /run/glov/secrets hold exactly its
// granted tmpfs secrets. A VM whose harnesses declare none is left alone.
// One sync per VM at a time, so an older list never lands after a newer
// one. Its error names secrets, never a value.
func (d *Daemon) syncSecrets(owner, name string) error {
	m, _ := d.secretSyncs.LoadOrStore(name, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	defer m.(*sync.Mutex).Unlock()
	r, err := d.Store.Load(owner, name)
	if err != nil || !r.Isolated || r.ID == "" || !d.VMs.Running(r.ID) {
		return nil
	}
	declared := tmpfsDeclared(r)
	if len(declared) == 0 || d.Secrets == nil {
		return nil
	}
	defined, _ := d.definedNames()
	list := []guestSecret{}
	var names []string
	if r.Egress != nil {
		for _, n := range r.Egress.Secrets {
			if !slices.Contains(declared, n) || !slices.Contains(defined, n) {
				continue
			}
			if r.Egress.Open && !slices.Contains(r.Egress.PlainSecrets, n) {
				slog.Warn("tmpfs secret withheld: the VM is open and the owner did not allow it (--allow-plain-secrets)", "vm", name, "secret", n)
				continue
			}
			v, err := d.Secrets.Value(n)
			if err != nil {
				slog.Warn("tmpfs secret left out", "vm", name, "err", err)
				continue
			}
			list = append(list, guestSecret{Name: n, Data: []byte(v)})
			names = append(names, n)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	params := struct {
		Owner   string        `json:"owner"`
		Secrets []guestSecret `json:"secrets"`
	}{provision.User, list}
	if err := d.VMs.GuestCall(ctx, r.ID, "secrets.sync", params, nil); err != nil {
		slog.Warn("cannot deliver tmpfs secrets", "vm", name, "secrets", names, "err", err)
		return fmt.Errorf("cannot deliver tmpfs secrets (%s): %v", strings.Join(names, ", "), err)
	}
	slog.Info("tmpfs secrets synced", "vm", name, "secrets", names)
	return nil
}

// resyncSecrets syncs each running VM in names, reporting what failed.
func (d *Daemon) resyncSecrets(owner string, names []string) error {
	var problems []string
	for _, n := range names {
		if err := d.syncSecrets(owner, n); err != nil {
			problems = append(problems, n+": "+err.Error())
		}
		if err := d.syncGitSignIns(owner, n); err != nil {
			problems = append(problems, n+": "+err.Error())
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("saved, but %s; the next Guest Service session retries, or stop the VM to clear its tmpfs", strings.Join(problems, "; "))
	}
	return nil
}
