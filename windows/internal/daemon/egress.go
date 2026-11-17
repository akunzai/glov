package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/store"
)

// proxy is the egress proxy of r's isolated VM: its settings are vm.json's,
// re-read for every connection, and its records go to the VM's egress.log
// and glovd's log. It puts granted secrets in place of placeholders, on
// connections it intercepts with the egress CA.
func (d *Daemon) proxy(r store.Record) *egress.Proxy {
	owner, name := r.Owner, r.Name
	p := &egress.Proxy{
		VM: name,
		Settings: func() (egress.Settings, error) {
			cur, err := d.Store.Load(owner, name)
			if err != nil {
				return egress.Settings{}, err
			}
			if !cur.Isolated || cur.Egress == nil {
				return egress.Settings{}, nil
			}
			return *cur.Egress, nil
		},
		Log: egress.LogTo(name, filepath.Join(d.Store.Dir(name), egress.LogFile)),
	}
	if d.Secrets != nil {
		p.Secrets, p.Value = d.Secrets.Egress, d.Secrets.Value
		p.TokenEndpoints = func(subscriptions []string) []egress.TokenEndpoint { return tokenEndpoints(name, subscriptions) }
		p.Keep = func(secret string, hosts []string, value string) error {
			_, err := d.Secrets.Set(secret, hosts, value, nil, "")
			return err
		}
		if t, err := d.egressTLS(); err != nil {
			slog.Warn("egress CA: the VM's secrets stay placeholders", "vm", name, "err", err)
		} else {
			p.TLS = t
		}
	}
	return p
}

// isolated loads caller's VM ref, which must be isolated.
func (d *Daemon) isolated(caller, ref string) (store.Record, error) {
	r, err := d.load(caller, ref)
	if err != nil {
		return store.Record{}, err
	}
	return r, notIsolated(r)
}

// lockedIsolated is isolated under the VM's lock, as locked; unlock is nil
// on error.
func (d *Daemon) lockedIsolated(caller, ref string) (store.Record, func(), error) {
	r, unlock, err := d.locked(caller, ref)
	if err != nil {
		return store.Record{}, nil, err
	}
	if err := notIsolated(r); err != nil {
		unlock()
		return store.Record{}, nil, err
	}
	return r, unlock, nil
}

func notIsolated(r store.Record) error {
	if !r.Isolated {
		return fmt.Errorf("VM %s is not isolated: egress policy applies only to VMs created with glov vm create --isolated", r.Name)
	}
	return nil
}

func egressOf(r store.Record) api.Egress {
	e := api.Egress{VM: r.Name, Allow: []string{}}
	if r.Egress != nil {
		e.Allow, e.MCPEdits, e.Private, e.Open = r.Egress.Allow, r.Egress.MCPEdits, r.Egress.PrivateAllow, r.Egress.Open
	}
	if e.Allow == nil {
		e.Allow = []string{}
	}
	return e
}

func (d *Daemon) egressList(caller string, p api.EgressParams) (api.Egress, error) {
	r, err := d.isolated(caller, p.VM)
	if err != nil {
		return api.Egress{}, err
	}
	return egressOf(r), nil
}

// editEgress changes caller's isolated VM's policy under its lock; the
// running proxy sees the change on its next connection.
func (d *Daemon) editEgress(caller, ref string, edit func(name string, e *store.Egress) error) (api.Egress, error) {
	r, unlock, err := d.lockedIsolated(caller, ref)
	if err != nil {
		return api.Egress{}, err
	}
	defer unlock()
	e := store.Egress{Allow: []string{}}
	if r.Egress != nil {
		e = *r.Egress
	}
	if err := edit(r.Name, &e); err != nil {
		return api.Egress{}, err
	}
	r.Egress = &e
	if err := d.Store.Save(r); err != nil {
		return api.Egress{}, err
	}
	return egressOf(r), nil
}

func (d *Daemon) egressEdit(caller string, p api.EgressParams, allow bool) (api.Egress, error) {
	if len(p.Hosts) == 0 {
		return api.Egress{}, proto.Errorf(proto.CodeInvalidParams, "hosts: name at least one host")
	}
	return d.editEgress(caller, p.VM, func(name string, e *store.Egress) error {
		if p.MCP && !e.MCPEdits {
			verb := "remove"
			if allow {
				verb = "allow"
			}
			return fmt.Errorf("VM %s does not let MCP clients change its egress allowlist: ask the user to run glov egress %s %s ..., or to opt in with glov egress mcp %s on", name, verb, name, name)
		}
		if p.Private && p.MCP {
			return fmt.Errorf("MCP clients cannot allow private-network hosts: ask the user to run glov egress allow %s <host> --private", name)
		}
		edit := egress.Removing
		if allow {
			edit = egress.Allowing
		}
		list, err := edit(e.Allow, p.Hosts)
		marked := e.PrivateAllow
		switch {
		case err != nil:
		case !allow:
			marked = egress.Unmarking(marked, p.Hosts)
		case p.Private:
			marked, err = egress.MarkingPrivate(marked, p.Hosts)
		}
		if errors.Is(err, egress.ErrInvalid) || errors.Is(err, egress.ErrPrivateSuffix) {
			return proto.Errorf(proto.CodeInvalidParams, "%v", err)
		}
		if err != nil {
			return err
		}
		e.Allow, e.PrivateAllow = list, marked
		return nil
	})
}

// egressMode opens caller's isolated VM to any public host, or restricts it
// to its allowlist again. Opening a VM that holds tmpfs secrets needs
// AllowPlainSecrets; restricting forgets that permission.
func (d *Daemon) egressMode(caller string, p api.EgressModeParams) (api.Egress, error) {
	res, err := d.editEgress(caller, p.VM, func(name string, e *store.Egress) error {
		if !p.Open {
			e.Open, e.PlainSecrets = false, nil
			return nil
		}
		plain, err := d.tmpfsAmong(caller, name, e.Secrets)
		if err != nil {
			return err
		}
		if len(plain) > 0 && !p.AllowPlainSecrets {
			return plainSecretsRefusal(name, plain, "glov vm configure "+name+" --egress open --allow-plain-secrets")
		}
		if len(plain) == 0 {
			plain = nil
		}
		e.Open, e.PlainSecrets = true, plain
		return nil
	})
	if err != nil {
		return api.Egress{}, err
	}
	return res, d.resyncSecrets(caller, []string{res.VM})
}

// plainSecretsRefusal is the error a terminal glov asks about before
// repeating the call with AllowPlainSecrets.
func plainSecretsRefusal(name string, plain []string, retry string) error {
	return proto.Errorf(api.CodePlainSecrets, "VM %s would hold %s as real values in tmpfs while open, and then any host could receive them; if that is acceptable, run %s", name, strings.Join(plain, ", "), retry)
}

func (d *Daemon) egressMCP(caller string, p api.EgressMCPParams) (api.Egress, error) {
	return d.editEgress(caller, p.VM, func(_ string, e *store.Egress) error {
		e.MCPEdits = p.Enabled
		return nil
	})
}

func (d *Daemon) egressLog(caller string, p api.EgressLogParams) (egress.Page, error) {
	r, err := d.isolated(caller, p.VM)
	if err != nil {
		return egress.Page{}, err
	}
	limit := p.Limit
	if limit == 0 {
		limit = api.DefaultEgressLogLimit
	}
	limit = min(max(limit, 1), api.MaxEgressLogLimit)
	return egress.Read(filepath.Join(d.Store.Dir(r.Name), egress.LogFile), p.Cursor, p.Denied, limit), nil
}
