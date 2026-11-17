package daemon

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/secret"
	"github.com/akunzai/glov/windows/internal/store"
)

func TestCreateIsolatedRecordsAnEmptyAllowlistAndProvisionsTheProxy(t *testing.T) {
	me := testOwner
	cr := &creator{st: store.Store{Root: t.TempDir()}}
	path, _ := serve(t, cr.daemon(me))
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "box", Isolated: true}, nil); err != nil {
		t.Fatal(err)
	}
	r, err := cr.st.Load(me, "box")
	if err != nil || !r.Isolated || r.Egress == nil || r.Egress.Allow == nil || len(r.Egress.Allow) != 0 || !cr.vm.Isolated {
		t.Fatalf("record %+v (egress %+v), provisioned %+v, %v", r, r.Egress, cr.vm, err)
	}
	d := &Daemon{Store: cr.st}
	if s := d.spec(r); s.Egress == nil || s.Egress.VM != "box" {
		t.Errorf("spec of an isolated VM: %+v", s)
	}
	if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "plain"}, nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := cr.st.Load(me, "plain"); !r.Isolated || r.Egress == nil || d.spec(r).Egress == nil {
		t.Errorf("a VM created without isolated is not isolated: %+v", r)
	}
}

func TestEgressEditsChangeTheAllowlistTheProxyReads(t *testing.T) {
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	for _, r := range []store.Record{{Name: "box", Owner: me, Isolated: true}, {Name: "plain", Owner: me}} {
		if err := st.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{Owner: me, Store: st, Version: "test", VMs: newFakeRunner()}
	path, _ := serve(t, d)
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(method string, params any) (api.Egress, error) {
		var e api.Egress
		err := c.Call(t.Context(), method, params, &e)
		return e, err
	}
	if e, err := call(api.MethodEgressList, api.EgressParams{VM: "bo"}); err != nil || e.VM != "box" || e.Allow == nil || len(e.Allow) != 0 || e.MCPEdits {
		t.Fatalf("list = %+v, %v", e, err)
	}
	e, err := call(api.MethodEgressAllow, api.EgressParams{VM: "box", Hosts: []string{"API.example.com.", "*.ubuntu.com"}})
	if err != nil || !slices.Equal(e.Allow, []string{"api.example.com", "*.ubuntu.com"}) {
		t.Fatalf("allow = %+v, %v", e, err)
	}
	proxy := d.proxy(store.Record{Name: "box", Owner: me})
	if s, err := proxy.Settings(); err != nil || !slices.Equal(s.Allow, e.Allow) {
		t.Errorf("the proxy reads %q, %v", s.Allow, err)
	}
	_, err = call(api.MethodEgressAllow, api.EgressParams{VM: "box", Hosts: []string{"a b"}})
	if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeInvalidParams || !strings.Contains(err.Error(), `"a b"`) {
		t.Errorf("invalid entry err = %v", err)
	}
	if _, err := call(api.MethodEgressAllow, api.EgressParams{VM: "box"}); err == nil {
		t.Error("allow without hosts succeeded")
	}
	// MCP clients change nothing until the user opts the VM in.
	_, err = call(api.MethodEgressAllow, api.EgressParams{VM: "box", Hosts: []string{"evil.example.net"}, MCP: true})
	if err == nil || !strings.Contains(err.Error(), "glov egress mcp box on") {
		t.Errorf("MCP allow before opt-in err = %v", err)
	}
	if e, err := call(api.MethodEgressMCP, api.EgressMCPParams{VM: "box", Enabled: true}); err != nil || !e.MCPEdits {
		t.Fatalf("mcp on = %+v, %v", e, err)
	}
	if e, err := call(api.MethodEgressRemove, api.EgressParams{VM: "box", Hosts: []string{"api.example.com"}, MCP: true}); err != nil || !slices.Equal(e.Allow, []string{"*.ubuntu.com"}) {
		t.Errorf("MCP remove after opt-in = %+v, %v", e, err)
	}
	if _, err := call(api.MethodEgressRemove, api.EgressParams{VM: "box", Hosts: []string{"x.com"}}); err == nil || !strings.Contains(err.Error(), "not in the allowlist: x.com") {
		t.Errorf("remove missing err = %v", err)
	}
	if s, _ := proxy.Settings(); !slices.Equal(s.Allow, []string{"*.ubuntu.com"}) {
		t.Errorf("the proxy reads %q after remove", s.Allow)
	}
	for _, m := range []string{api.MethodEgressList, api.MethodEgressAllow} {
		if _, err := call(m, api.EgressParams{VM: "plain", Hosts: []string{"a.com"}}); err == nil || !strings.Contains(err.Error(), "is not isolated") {
			t.Errorf("%s on a normal VM err = %v", m, err)
		}
	}
}

func TestEgressLogReadsTheVMsRecords(t *testing.T) {
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: me, Isolated: true}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Owner: me, Store: st, Version: "test", VMs: newFakeRunner()}
	log := d.proxy(store.Record{Name: "box", Owner: me}).Log
	log(egress.NewRecord("box", egress.Allow, "a.com", 443, ""))
	log(egress.NewRecord("box", egress.Deny, "b.com", 443, "not in the allowlist"))
	if _, err := os.Stat(filepath.Join(st.Dir("box"), egress.LogFile)); err != nil {
		t.Fatal(err)
	}
	path, _ := serve(t, d)
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var page egress.Page
	if err := c.Call(t.Context(), api.MethodEgressLog, api.EgressLogParams{VM: "box", Denied: true}, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].Summary() != "deny b.com:443 in=0 out=0 (not in the allowlist)" || page.Cursor == "" {
		t.Fatalf("page = %+v", page)
	}
	var next egress.Page
	if err := c.Call(t.Context(), api.MethodEgressLog, api.EgressLogParams{VM: "box", Cursor: page.Cursor}, &next); err != nil || len(next.Records) != 0 {
		t.Fatalf("after the cursor: %+v, %v", next, err)
	}
}

func TestSecretsAreGrantedAndRemovedWithoutAValueEverComingBack(t *testing.T) {
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	for _, r := range []store.Record{{Name: "box", Owner: me, Isolated: true, ID: "id-box"}, {Name: "plain", Owner: me}} {
		if err := st.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	runner := newFakeRunner()
	d := &Daemon{Owner: me, Store: st, Version: "test", VMs: runner, CADir: filepath.Join(st.Root, "egress-ca"),
		Secrets: &secret.Store{Dir: filepath.Join(st.Root, "secrets"), Values: &secret.Memory{}}}
	path, _ := serve(t, d)
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := t.Context()

	const value = "sk-ant-TEST-SECRET-0123456789"
	var got api.Secret
	if err := c.Call(ctx, api.MethodSecretSet, api.SecretParams{Name: "anthropic", Hosts: []string{"API.anthropic.com"}, Value: value}, &got); err != nil ||
		got.Source != secret.Stored || !slices.Equal(got.Hosts, []string{"api.anthropic.com"}) || got.Length != len(value) || got.Ending != "6789" {
		t.Fatalf("set = %+v, %v", got, err)
	}
	for _, bad := range []api.SecretParams{{Name: "a/b", Hosts: []string{"x.com"}, Value: "x"}, {Name: "n", Hosts: []string{"x.com"}}, {Name: "n", Value: "x"}, {Value: "x"}} {
		if err := c.Call(ctx, api.MethodSecretSet, bad, nil); err == nil {
			t.Errorf("set %+v succeeded", bad.Name)
		}
	}
	if err := c.Call(ctx, api.MethodSecretSource, api.SecretParams{Name: "gh", Hosts: []string{"github.com"}, Value: "gho_x", Command: []string{"gh", "auth", "token"}}, nil); err == nil ||
		!strings.Contains(err.Error(), "absolute path") {
		t.Errorf("source with a relative command err = %v", err)
	}
	if err := c.Call(ctx, api.MethodSecretSource, api.SecretParams{Name: "gh", Hosts: []string{"github.com"}, Value: "gho_x", Command: []string{filepath.Join(t.TempDir(), "gh.exe"), "auth", "token"}}, func() *api.Secret { got = api.Secret{}; return &got }()); err != nil ||
		got.Source != secret.Command || got.Length != 0 {
		t.Errorf("source = %+v, %v", got, err)
	}
	if err := c.Call(ctx, api.MethodSecretRefresh, api.SecretParams{Name: "anthropic", Value: "v"}, nil); err == nil || !strings.Contains(err.Error(), "not taken from a command") {
		t.Errorf("refresh of a stored secret err = %v", err)
	}
	var list api.Secrets
	if err := c.Call(ctx, api.MethodSecretList, struct{}{}, &list); err != nil || len(list.Secrets) != 2 || list.Secrets[0].Name != "anthropic" {
		t.Errorf("list = %+v, %v", list, err)
	}

	var g api.SecretGrants
	if err := c.Call(ctx, api.MethodSecretGrant, api.SecretGrantParams{VM: "box", Names: []string{"nope"}}, &g); err == nil || !strings.Contains(err.Error(), "no secret named nope") {
		t.Errorf("grant of an undefined secret err = %v", err)
	}
	if err := c.Call(ctx, api.MethodSecretGrant, api.SecretGrantParams{VM: "plain", Names: []string{"gh"}}, &g); err == nil {
		t.Error("a normal VM was granted a secret")
	}
	if err := c.Call(ctx, api.MethodSecretGrant, api.SecretGrantParams{VM: "box", Names: []string{"anthropic", "gh", "anthropic"}}, &g); err != nil ||
		!slices.Equal(g.Names, []string{"anthropic", "gh"}) || len(g.Missing) != 0 {
		t.Errorf("grant = %+v, %v", g, err)
	}
	p := d.proxy(store.Record{Name: "box", Owner: me})
	if s, err := p.Settings(); err != nil || !slices.Equal(s.Secrets, []string{"anthropic", "gh"}) || p.TLS == nil || p.Value == nil {
		t.Errorf("the proxy reads %+v, %v (TLS %v)", s.Secrets, err, p.TLS != nil)
	}
	if v, err := p.Value("anthropic"); err != nil || v != value {
		t.Error("the proxy cannot read the value")
	}
	if err := c.Call(ctx, api.MethodSecretRevoke, api.SecretGrantParams{VM: "box", Names: []string{"other"}}, &g); err == nil || !strings.Contains(err.Error(), "was not granted other") {
		t.Errorf("revoke of an ungranted secret err = %v", err)
	}
	if err := c.Call(ctx, api.MethodSecretRemove, api.SecretParams{Name: "gh"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, api.MethodSecretGrants, api.SecretGrantParams{VM: "box"}, &g); err != nil || !slices.Equal(g.Names, []string{"anthropic"}) {
		t.Errorf("grants after removing gh = %+v, %v", g, err)
	}
	if err := c.Call(ctx, api.MethodSecretRemove, api.SecretParams{Name: "gh"}, nil); err == nil || !strings.Contains(err.Error(), "no secret named gh") {
		t.Errorf("second remove err = %v", err)
	}
	g = api.SecretGrants{}
	if err := c.Call(ctx, api.MethodSecretRevoke, api.SecretGrantParams{VM: "box", Names: []string{"anthropic"}}, &g); err != nil || len(g.Names) != 0 || g.Names == nil {
		t.Errorf("revoke = %+v, %v", g, err)
	}
	if r, _ := st.Load(me, "box"); r.Egress.Secrets != nil {
		t.Errorf("an empty grant list is kept: %#v", r.Egress.Secrets)
	}
	logged, _ := os.ReadFile(filepath.Join(st.Root, "glovd.log"))
	if strings.Contains(string(logged), value) {
		t.Error("glovd's log holds the value")
	}
}

func TestARunningVMGetsExactlyItsGrantedTmpfsSecrets(t *testing.T) {
	namedProfile = func(name string) (harness.Profile, error) {
		return harness.Profile{Name: name, Secrets: []harness.Secret{
			{Name: "raw", Delivery: harness.Tmpfs, File: ".h/token", Hosts: []string{"auth.example.com"}},
			{Name: "api", Delivery: harness.Placeholder, Env: "H_KEY", Hosts: []string{"api.example.com"}},
		}}, nil
	}
	t.Cleanup(func() { namedProfile = harness.Named })
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: me, Isolated: true, ID: "id-box", Harnesses: []string{"h"}, Egress: &store.Egress{Allow: []string{}}}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	// The git sign-in sync's commands, which write nothing without git secrets.
	runner.execOut = func([]string) (string, int) { return "", 0 }
	d := &Daemon{Owner: me, Store: st, VMs: runner, Secrets: &secret.Store{Dir: t.TempDir(), Values: &secret.Memory{}}}
	for _, n := range []string{"raw", "api"} {
		if _, err := d.Secrets.Set(n, []string{"example.com"}, "value-of-"+n, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.secretGrant(me, api.SecretGrantParams{VM: "box", Names: []string{"raw", "api"}}, true); err != nil || len(runner.secrets) != 0 {
		t.Fatalf("a stopped VM was synced: %d, %v", len(runner.secrets), err)
	}
	runner.running["id-box"] = true
	r, err := st.Load(me, "box")
	if err != nil {
		t.Fatal(err)
	}
	d.spec(r).OnReady()
	if len(runner.secrets) != 1 {
		t.Fatalf("on ready the guest got %d syncs", len(runner.secrets))
	}
	sent, _ := json.Marshal(runner.secrets[0])
	if string(sent) != `{"owner":"glov","secrets":[{"name":"raw","data":"dmFsdWUtb2YtcmF3"}]}` {
		t.Errorf("on ready the guest got %s", sent)
	}
	runner.secretsFail = errors.New("no tmpfs")
	_, err = d.secretGrant(me, api.SecretGrantParams{VM: "box", Names: []string{"raw"}}, false)
	if err == nil || err.Error() != "saved, but box: cannot deliver tmpfs secrets (): no tmpfs; the next Guest Service session retries, or stop the VM to clear its tmpfs" {
		t.Errorf("revoke with a failing guest err = %v", err)
	}
	if r, _ := st.Load(me, "box"); !slices.Equal(r.Egress.Secrets, []string{"api"}) {
		t.Errorf("grants after revoke = %v", r.Egress.Secrets)
	}
}

func TestCreateWithHarnessesAllowsTheirRuntimeHostsAndTrustsTheEgressCA(t *testing.T) {
	me := testOwner
	cr := &creator{st: store.Store{Root: t.TempDir()}}
	path, _ := serve(t, cr.daemon(me))
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var vm api.VMInfo
	if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "agent", Isolated: true, Harnesses: []string{"claude", "codex", "claude"}}, &vm); err != nil {
		t.Fatal(err)
	}
	r, _ := cr.st.Load(me, "agent")
	if !slices.Equal(r.Harnesses, []string{"claude", "codex"}) || !slices.Equal(vm.Harnesses, r.Harnesses) {
		t.Errorf("harnesses: record %q, answer %q", r.Harnesses, vm.Harnesses)
	}
	if !slices.Equal(r.Egress.Allow, []string{"api.anthropic.com", "api.openai.com"}) || r.Egress.Secrets != nil {
		t.Errorf("egress = %+v", r.Egress)
	}
	if !strings.HasPrefix(string(cr.vm.EgressCA), "-----BEGIN CERTIFICATE-----") {
		t.Error("the VM was not provisioned to trust the egress CA")
	}
	if len(cr.vm.Harnesses) != 2 || cr.vm.Harnesses[1].Name != "codex" {
		t.Errorf("provisioned with %+v", cr.vm.Harnesses)
	}
	for _, bad := range []api.CreateParams{{VM: "y", Harnesses: []string{"aider"}}} {
		if err := c.Call(t.Context(), api.MethodVMCreate, bad, nil); err == nil {
			t.Errorf("create %+v succeeded", bad)
		}
	}
}
