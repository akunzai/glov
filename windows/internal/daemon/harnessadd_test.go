package daemon

import (
	"encoding/base64"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/store"
)

func TestAHarnessIsAddedToARunningVMKeepingTheOwnersFiles(t *testing.T) {
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: testOwner, Isolated: true, ID: "id-box", Harnesses: []string{"claude"},
		Egress: &store.Egress{Allow: []string{"api.anthropic.com"}}}); err != nil {
		t.Fatal(err)
	}
	claude, _ := harness.Resolve([]string{"claude"})
	g := &guestFiles{files: map[string]string{
		"/etc/environment": harness.Environment("PATH=/usr/bin\n", claude, nil, "box"),
		codexConfig:        "# the owner's own\n",
	}}
	var ran [][]string
	runner := newFakeRunner()
	runner.running["id-box"] = true
	runner.execOut = func(argv []string) (string, int) {
		ran = append(ran, argv)
		if i := slices.Index(argv, writeNewScript); i >= 0 {
			out := ""
			for args := argv[i+2:]; len(args) >= 2; args = args[2:] {
				p := path.Join("/home", provision.User, args[0])
				if _, ok := g.files[p]; ok {
					out += "kept " + args[0] + "\n"
					continue
				}
				data, _ := base64.StdEncoding.DecodeString(args[1])
				g.set(p, string(data))
			}
			return out, 0
		}
		return g.exec(argv)
	}
	d := &Daemon{Owner: testOwner, Store: st, VMs: runner}

	res, err := d.harnessAdd(testOwner, api.HarnessAddParams{VM: "bo", Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if res.VM != "box" || res.Already || !slices.Equal(res.Kept, []string{".codex/config.toml"}) || !slices.Contains(res.Allowed, "api.openai.com") {
		t.Errorf("result %+v", res)
	}
	if g.get(codexConfig) != "# the owner's own\n" {
		t.Error("the owner's codex config was overwritten")
	}
	if env := g.get("/etc/environment"); !strings.Contains(env, "PATH=/usr/bin") || !strings.Contains(env, "CODEX_API_KEY=") {
		t.Errorf("environment %q", env)
	}
	if !strings.Contains(g.get(provision.UserNamespacesPath), "apparmor_restrict_unprivileged_userns = 0") ||
		!slices.ContainsFunc(ran, func(a []string) bool {
			return slices.Equal(a, []string{"sysctl", "-e", "-p", provision.UserNamespacesPath})
		}) {
		t.Error("codex's user namespaces were not allowed")
	}
	r, _ := st.Load(testOwner, "box")
	if !slices.Equal(r.Harnesses, []string{"claude", "codex"}) || !slices.Contains(r.Egress.Allow, "api.openai.com") || !slices.Contains(r.Egress.Allow, "api.anthropic.com") {
		t.Errorf("record harnesses %q, allow %q", r.Harnesses, r.Egress.Allow)
	}
	if again, err := d.harnessAdd(testOwner, api.HarnessAddParams{VM: "box", Harness: "codex"}); err != nil || !again.Already {
		t.Errorf("adding it again = %+v, %v", again, err)
	}
	if _, err := d.harnessAdd(testOwner, api.HarnessAddParams{VM: "box", Harness: "aider"}); err == nil {
		t.Error("an unknown harness was added")
	}

	r.Template = &store.Template{}
	st.Save(r)
	if _, err := d.harnessAdd(testOwner, api.HarnessAddParams{VM: "box", Harness: "antigravity"}); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Errorf("adding to a frozen VM = %v", err)
	}
	r.Template = nil
	st.Save(r)
	runner.running["id-box"] = false
	if _, err := d.harnessAdd(testOwner, api.HarnessAddParams{VM: "box", Harness: "antigravity"}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("adding to a stopped VM = %v", err)
	}
}

// A VM created without --harness has no guest user for one: refused before
// anything runs in the guest, saying how to get a harness VM.
func TestAHarnessIsNotAddedToAVMCreatedWithoutOne(t *testing.T) {
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "plain", Owner: testOwner, Isolated: true, ID: "id-plain"}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	runner.running["id-plain"] = true
	ran := false
	runner.execOut = func([]string) (string, int) { ran = true; return "", 0 }
	d := &Daemon{Owner: testOwner, Store: st, VMs: runner}

	_, err := d.harnessAdd(testOwner, api.HarnessAddParams{VM: "plain", Harness: "claude"})
	want := "VM plain was created without --harness, which glov vm harness add cannot make up for: create a harness VM with glov vm create <vm> --isolated --harness claude"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if ran {
		t.Error("something ran in the guest")
	}
	if r, _ := st.Load(testOwner, "plain"); len(r.Harnesses) != 0 {
		t.Errorf("record harnesses %q", r.Harnesses)
	}
}
