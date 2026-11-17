package daemon

import (
	"encoding/json"
	"errors"
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

func TestPrivateEntriesAreMarkedByTheCLIOnlyAndUnmarkedOnRemove(t *testing.T) {
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: me, Isolated: true, Egress: &store.Egress{Allow: []string{}, MCPEdits: true}}); err != nil {
		t.Fatal(err)
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
	e, err := call(api.MethodEgressAllow, api.EgressParams{VM: "box", Hosts: []string{"GitLab.corp.example", "10.1.2.3:8443"}, Private: true})
	if err != nil || !slices.Equal(e.Allow, []string{"gitlab.corp.example", "10.1.2.3:8443"}) || !slices.Equal(e.Private, e.Allow) {
		t.Fatalf("allow --private = %+v, %v", e, err)
	}
	if _, err := call(api.MethodEgressAllow, api.EgressParams{VM: "box", Hosts: []string{"db.corp.example"}, Private: true, MCP: true}); err == nil ||
		!strings.Contains(err.Error(), "MCP clients cannot allow private-network hosts: ask the user to run glov egress allow box <host> --private") {
		t.Errorf("MCP --private err = %v", err)
	}
	_, err = call(api.MethodEgressAllow, api.EgressParams{VM: "box", Hosts: []string{"ok.example.com", "*.corp.example"}, Private: true})
	if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != proto.CodeInvalidParams || !strings.Contains(err.Error(), "not *.corp.example") {
		t.Errorf("a private suffix err = %v", err)
	}
	if e, err := call(api.MethodEgressAllow, api.EgressParams{VM: "box", Hosts: []string{"gitlab.corp.example", "api.example.com"}}); err != nil ||
		!slices.Equal(e.Private, []string{"gitlab.corp.example", "10.1.2.3:8443"}) || len(e.Allow) != 3 {
		t.Errorf("allowing again without --private = %+v, %v", e, err)
	}
	if e, err := call(api.MethodEgressRemove, api.EgressParams{VM: "box", Hosts: []string{"GITLAB.corp.example", "10.1.2.3:8443"}}); err != nil ||
		!slices.Equal(e.Allow, []string{"api.example.com"}) || e.Private != nil {
		t.Errorf("remove = %+v, %v", e, err)
	}
	if r, _ := st.Load(me, "box"); r.Egress.PrivateAllow != nil {
		t.Errorf("vm.json keeps privateAllow %q", r.Egress.PrivateAllow)
	}
}

// tmpfsProfile makes every harness declare raw as a tmpfs secret and api as
// a placeholder one.
func tmpfsProfile(t *testing.T) {
	namedProfile = func(name string) (harness.Profile, error) {
		return harness.Profile{Name: name, Secrets: []harness.Secret{
			{Name: "raw", Delivery: harness.Tmpfs, File: ".h/token", Hosts: []string{"auth.example.com"}},
			{Name: "api", Delivery: harness.Placeholder, Env: "H_KEY", Hosts: []string{"api.example.com"}},
		}}, nil
	}
	t.Cleanup(func() { namedProfile = harness.Named })
}

func TestOpeningAVMThatHoldsTmpfsSecretsNeedsTheOwnersSayAndWithholdsTheRest(t *testing.T) {
	tmpfsProfile(t)
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: me, Isolated: true, ID: "id-box", Harnesses: []string{"h"},
		Egress: &store.Egress{Allow: []string{"api.example.com"}}}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	d := &Daemon{Owner: me, Store: st, Version: "test", VMs: runner, Secrets: &secret.Store{Dir: t.TempDir(), Values: &secret.Memory{}}}
	for _, n := range []string{"raw", "raw2", "api"} {
		if _, err := d.Secrets.Set(n, []string{"example.com"}, "value-of-"+n, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	path, _ := serve(t, d)
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mode := func(p api.EgressModeParams) (api.Egress, error) {
		var e api.Egress
		err := c.Call(t.Context(), api.MethodEgressMode, p, &e)
		return e, err
	}
	// Nothing in tmpfs yet: opening needs no permission.
	if e, err := mode(api.EgressModeParams{VM: "box", Open: true}); err != nil || !e.Open || !slices.Equal(e.Allow, []string{"api.example.com"}) {
		t.Fatalf("open = %+v, %v", e, err)
	}
	if s, _ := d.proxy(store.Record{Name: "box", Owner: me}).Settings(); !s.Permits("anything.example.net", 443) {
		t.Error("the proxy of an open VM does not permit any host")
	}
	// Granting a tmpfs secret to an open VM needs the owner's say.
	err = c.Call(t.Context(), api.MethodSecretGrant, api.SecretGrantParams{VM: "box", Names: []string{"api", "raw"}}, nil)
	if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != api.CodePlainSecrets ||
		pe.Message != "VM box would hold raw as real values in tmpfs while open, and then any host could receive them; if that is acceptable, run glov secret grant box api raw --allow-plain-secrets" {
		t.Fatalf("grant to an open VM err = %v", err)
	}
	if r, _ := st.Load(me, "box"); r.Egress.Secrets != nil {
		t.Errorf("a refused grant was saved: %q", r.Egress.Secrets)
	}
	if err := c.Call(t.Context(), api.MethodSecretGrant, api.SecretGrantParams{VM: "box", Names: []string{"api", "raw"}, AllowPlainSecrets: true}, nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.Load(me, "box"); !slices.Equal(r.Egress.PlainSecrets, []string{"raw"}) {
		t.Errorf("plainSecrets = %q", r.Egress.PlainSecrets)
	}
	// Restricting forgets the permission; opening again asks again.
	if e, err := mode(api.EgressModeParams{VM: "box"}); err != nil || e.Open {
		t.Fatalf("restrict = %+v, %v", e, err)
	}
	if r, _ := st.Load(me, "box"); r.Egress.PlainSecrets != nil || r.Egress.Open {
		t.Errorf("restricted, vm.json keeps %+v", r.Egress)
	}
	_, err = mode(api.EgressModeParams{VM: "box", Open: true})
	if pe, ok := errors.AsType[*proto.Error](err); !ok || pe.Code != api.CodePlainSecrets ||
		!strings.HasSuffix(pe.Message, "run glov vm configure box --egress open --allow-plain-secrets") {
		t.Fatalf("open with a tmpfs secret err = %v", err)
	}
	if e, err := mode(api.EgressModeParams{VM: "box", Open: true, AllowPlainSecrets: true}); err != nil || !e.Open {
		t.Fatalf("open --allow-plain-secrets = %+v, %v", e, err)
	}
	// A tmpfs secret the owner did not allow while open is withheld.
	if _, err := d.editEgress(me, "box", func(_ string, e *store.Egress) error { e.PlainSecrets = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	runner.running["id-box"] = true
	if err := d.syncSecrets(me, "box"); err != nil {
		t.Fatal(err)
	}
	if sent, _ := json.Marshal(runner.secrets[len(runner.secrets)-1]); string(sent) != `{"owner":"glov","secrets":[]}` {
		t.Errorf("an open VM got %s", sent)
	}
	if err := c.Call(t.Context(), api.MethodEgressMode, api.EgressModeParams{VM: "nope", Open: true}, nil); err == nil {
		t.Error("egress.mode on an unknown VM succeeded")
	}
}

func TestCreateOpenWithoutIsolated(t *testing.T) {
	me := testOwner
	cr := &creator{st: store.Store{Root: t.TempDir()}}
	path, _ := serve(t, cr.daemon(me))
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "agent", Open: true}, nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := cr.st.Load(me, "agent"); !r.Isolated || r.Egress == nil || !r.Egress.Open {
		t.Errorf("record %+v, egress %+v", r, r.Egress)
	}
}

func testCA(t *testing.T) string {
	ca, err := egress.LoadOrCreateCA(filepath.Join(t.TempDir(), egress.CADir))
	if err != nil {
		t.Fatal(err)
	}
	return string(ca.PEM)
}

func TestTrustedCertificatesAreCheckedStoredAndSyncedToARunningGuest(t *testing.T) {
	me := testOwner
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: me, Isolated: true, ID: "id-box", Egress: &store.Egress{Allow: []string{}}}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	runner.execOut = func([]string) (string, int) { return "changed\n", 0 }
	d := &Daemon{Owner: me, Store: st, Version: "test", VMs: runner}
	path, _ := serve(t, d)
	c, err := dial(t, path, me)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(method string, p api.TrustParams) (api.Trust, error) {
		var res api.Trust
		err := c.Call(t.Context(), method, p, &res)
		return res, err
	}
	one, other := testCA(t), testCA(t)
	for _, bad := range []struct{ name, pem, want string }{
		{"../x", one, `trust name "../x": use A-Z a-z 0-9 . _ - (1 to 64 characters)`},
		{"..", one, "1 to 64 characters"},
		{"corp", "hello", "no PEM certificate (-----BEGIN CERTIFICATE-----) in the file"},
		{"corp", one + other, "2 certificates in the file: add one CA per name"},
		{"corp", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", "not a readable certificate: "},
	} {
		if _, err := call(api.MethodTrustAdd, api.TrustParams{VM: "box", Name: bad.name, Certificate: bad.pem}); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("add %q: %v", bad.name, err)
		}
	}
	if res, err := call(api.MethodTrustList, api.TrustParams{VM: "box"}); err != nil || res.Certificates == nil || len(res.Certificates) != 0 {
		t.Fatalf("empty list = %+v, %v", res, err)
	}
	// Stopped: saved, synced at the next Guest Service session.
	res, err := call(api.MethodTrustAdd, api.TrustParams{VM: "box", Name: "corp", Certificate: "subject: x\n" + one})
	if err != nil || len(res.Certificates) != 1 || len(runner.execs) != 0 {
		t.Fatalf("add = %+v, %v, %d execs", res, err, len(runner.execs))
	}
	got := res.Certificates[0]
	if got.Name != "corp" || got.Subject != "CN=glov egress CA" || !got.Authority || len(got.SHA256) != 64 || got.NotAfter == nil {
		t.Errorf("summary %+v", got)
	}
	if r, _ := st.Load(me, "box"); r.Egress.Trust[0].Certificate != one {
		t.Errorf("stored %q", r.Egress.Trust[0].Certificate)
	}
	runner.running["id-box"] = true
	if res, err = call(api.MethodTrustAdd, api.TrustParams{VM: "box", Name: "corp", Certificate: other}); err != nil || len(res.Certificates) != 1 || res.Certificates[0].SHA256 == got.SHA256 {
		t.Fatalf("replace = %+v, %v", res, err)
	}
	if len(runner.execs) != 1 {
		t.Fatalf("%d execs", len(runner.execs))
	}
	argv := runner.execs[0].Argv
	if len(argv) != 3 || argv[0] != "sh" || runner.execs[0].As != "system" ||
		!strings.Contains(argv[2], "cat > \"$t/corp.crt\" <<'GLOV_PEM'\n"+other+"GLOV_PEM\n") || !strings.Contains(argv[2], "update-ca-certificates --fresh") {
		t.Errorf("sync ran %q as %s", argv, runner.execs[0].As)
	}
	if _, err := call(api.MethodTrustRemove, api.TrustParams{VM: "box", Name: "other"}); err == nil ||
		err.Error() != "rpc error 1: VM box has no trusted certificate named other: see glov vm trust list box" {
		t.Errorf("remove unknown: %v", err)
	}
	runner.execOut = func([]string) (string, int) { return "update-ca-certificates: not found\n", 127 }
	_, err = call(api.MethodTrustRemove, api.TrustParams{VM: "box", Name: "corp"})
	if err == nil || !strings.HasPrefix(err.Error(), "rpc error 1: saved, but cannot update the trust store: exit code 127: update-ca-certificates: not found") ||
		!strings.HasSuffix(err.Error(), "; the next Guest Service session retries") {
		t.Errorf("remove with a failing guest: %v", err)
	}
	if r, _ := st.Load(me, "box"); r.Egress.Trust != nil {
		t.Errorf("trust after remove = %+v", r.Egress.Trust)
	}
	if last := runner.execs[len(runner.execs)-1].Argv[2]; strings.Contains(last, "GLOV_PEM") {
		t.Errorf("an empty list still writes certificates: %q", last)
	}
}
