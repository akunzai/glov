package daemon

import (
	"encoding/base64"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/secret"
	"github.com/akunzai/glov/windows/internal/store"
)

// guestFiles is a fake guest's files, served to readGuestFile and written
// by putScript and userPutScript.
type guestFiles struct {
	mu    sync.Mutex
	files map[string]string
}

func (g *guestFiles) get(p string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.files[p]
}

func (g *guestFiles) set(p, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[p] = text
}

func (g *guestFiles) exec(argv []string) (string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(argv) == 5 && strings.Contains(argv[2], "base64") {
		return base64.StdEncoding.EncodeToString([]byte(g.files[argv[4]])) + "\n", 0
	}
	dir := ""
	args := argv
	switch i := max(slices.Index(argv, putScript), slices.Index(argv, userPutScript)); {
	case i < 0:
		return "", 0
	case argv[i] == userPutScript:
		dir = "/home/" + provision.User
		fallthrough
	default:
		args = argv[i+2:]
	}
	out := ""
	for ; len(args) >= 3; args = args[3:] {
		p := args[0]
		if dir != "" {
			p = path.Join(dir, p)
		}
		data, _ := base64.StdEncoding.DecodeString(args[2])
		if _, ok := g.files[p]; string(data) == "" && !ok {
			continue
		}
		if g.files[p] != string(data) {
			g.files[p] = string(data)
			out += "changed\n"
		}
	}
	return out, 0
}

const codexConfig = "/home/glov/.codex/config.toml"

// subscriptionDaemon is a glovd with running isolated VM box made with the
// claude and codex harnesses, its guest in API mode as provisioning left
// it, and the owner's own change to codex's config.
func subscriptionDaemon(t *testing.T) (*Daemon, *guestFiles) {
	t.Helper()
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: testOwner, Isolated: true, ID: "id-box", Harnesses: []string{"claude", "codex"},
		Egress: &store.Egress{Allow: []string{"api.openai.com"}}}); err != nil {
		t.Fatal(err)
	}
	profiles, _ := harness.Resolve([]string{"claude", "codex"})
	codex := profiles[1]
	i := slices.IndexFunc(codex.Files, func(f harness.File) bool { return f.Path == ".codex/config.toml" })
	g := &guestFiles{files: map[string]string{
		"/etc/environment": harness.Environment("PATH=/usr/bin\n", profiles, nil, "box"),
		codexConfig:        strings.Replace(codex.Files[i].Content(), `approval_policy = "on-request"`, `approval_policy = "never"`, 1),
	}}
	runner := newFakeRunner()
	runner.running["id-box"] = true
	runner.execOut = g.exec
	return &Daemon{Owner: testOwner, Store: st, VMs: runner, Secrets: &secret.Store{Dir: t.TempDir(), Values: &secret.Memory{}}}, g
}

func TestCodexSignsInInTheGuestAndOutAgainLeavingTheOwnersSettings(t *testing.T) {
	d, g := subscriptionDaemon(t)
	// No marker means API mode, so a session's own check leaves the guest
	// alone.
	before := g.get("/etc/environment")
	if err := d.syncHarnessMode(testOwner, "box", false); err != nil || g.get("/etc/environment") != before {
		t.Fatalf("session check: %v", err)
	}
	if _, ok := g.files[harness.MarkerPath]; ok {
		t.Fatal("a marker was written without a subscription")
	}
	login, err := d.secretLogin(testOwner, api.SecretLoginParams{VM: "box", Harness: "codex"})
	if err != nil || !slices.Equal(login.Command, []string{"codex", "login", "--device-auth"}) || !slices.Equal(login.Missing, []string{"box.codex.access", "box.codex.refresh"}) {
		t.Fatalf("login %+v, %v", login, err)
	}
	r, _ := d.Store.Load(testOwner, "box")
	if e := r.Egress; !slices.Equal(e.Subscriptions, []string{"codex"}) || !slices.Equal(e.Secrets, []string{"box.codex.access", "box.codex.refresh"}) ||
		!slices.Contains(e.Allow, "auth.openai.com") || !slices.Contains(e.Allow, "chatgpt.com") {
		t.Errorf("egress %+v", e)
	}
	env, config := g.get("/etc/environment"), g.get(codexConfig)
	if g.get(harness.MarkerPath) != "codex\n" || strings.Contains(env, "CODEX_API_KEY") || !strings.Contains(env, "PATH=/usr/bin") ||
		!strings.Contains(config, `model_provider = "glov-chatgpt"`) || !strings.Contains(config, `approval_policy = "never"`) {
		t.Errorf("signed in: marker %q, env %q, config %q", g.get(harness.MarkerPath), env, config)
	}
	if eps := tokenEndpoints("box", r.Egress.Subscriptions); len(eps) != 2 || eps[0].Host != "auth.openai.com" || eps[0].Path != "/oauth/token" ||
		eps[0].Answer["refresh_token"].Name != "box.codex.refresh" || !slices.Equal(eps[0].Answer["access_token"].Hosts, []string{"chatgpt.com"}) {
		t.Errorf("token endpoints %+v", eps)
	}

	if _, err := d.secretLogout(testOwner, api.SecretLoginParams{VM: "box", Harness: "codex"}); err != nil {
		t.Fatal(err)
	}
	r, _ = d.Store.Load(testOwner, "box")
	if e := r.Egress; e.Subscriptions != nil || e.Secrets != nil || !slices.Equal(e.Allow, []string{"api.openai.com"}) {
		t.Errorf("after logout %+v", e)
	}
	env, config = g.get("/etc/environment"), g.get(codexConfig)
	if !strings.Contains(env, "CODEX_API_KEY=glov-secret:openai") || strings.Contains(config, `model_provider = "glov-chatgpt"`) ||
		!strings.Contains(config, `approval_policy = "never"`) || g.get(harness.MarkerPath) != "" {
		t.Errorf("signed out: env %q, config %q", env, config)
	}
	if _, err := d.secretLogout(testOwner, api.SecretLoginParams{VM: "box", Harness: "codex"}); err == nil || !strings.Contains(err.Error(), "glov secret login box codex") {
		t.Errorf("a second logout: %v", err)
	}
}

func TestAPastedClaudeTokenIsStoredUnderTheVMAndTheGuestGetsItsPlaceholder(t *testing.T) {
	d, g := subscriptionDaemon(t)
	if _, err := d.secretLogin(testOwner, api.SecretLoginParams{VM: "box", Harness: "claude"}); err == nil || !strings.Contains(err.Error(), "claude setup-token") {
		t.Errorf("no token: %v", err)
	}
	login, err := d.secretLogin(testOwner, api.SecretLoginParams{VM: "box", Harness: "claude", Value: "sk-ant-oat-VALUE"})
	if err != nil || login.Command != nil || len(login.Missing) != 0 {
		t.Fatalf("login %+v, %v", login, err)
	}
	list, _ := d.secretList()
	if len(list.Secrets) != 1 || list.Secrets[0].Name != "box.claude.token" || !slices.Equal(list.Secrets[0].Hosts, []string{"api.anthropic.com"}) {
		t.Errorf("secrets %+v", list.Secrets)
	}
	env := g.get("/etc/environment")
	if !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=glov-secret:box.claude.token") || strings.Contains(env, "ANTHROPIC_API_KEY") || strings.Contains(env, "VALUE") {
		t.Errorf("env %q", env)
	}
	// A guest away at the switch (or reset to its template) catches up at
	// its next session.
	g.set(harness.MarkerPath, "")
	g.set("/etc/environment", "PATH=/usr/bin\n")
	if err := d.syncHarnessMode(testOwner, "box", false); err != nil {
		t.Fatal(err)
	}
	if g.get(harness.MarkerPath) != "claude\n" || !strings.Contains(g.get("/etc/environment"), "CLAUDE_CODE_OAUTH_TOKEN=glov-secret:box.claude.token") {
		t.Errorf("after the session: marker %q, env %q", g.get(harness.MarkerPath), g.get("/etc/environment"))
	}
	// Logout removes the token.
	if _, err := d.secretLogout(testOwner, api.SecretLoginParams{VM: "box", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.secretList(); len(list.Secrets) != 0 {
		t.Errorf("logout left %+v", list.Secrets)
	}
}

func TestAHarnessTheVMLacksIsRefused(t *testing.T) {
	d, _ := subscriptionDaemon(t)
	if _, err := d.secretLogin(testOwner, api.SecretLoginParams{VM: "box", Harness: "antigravity"}); err == nil || !strings.HasPrefix(err.Error(), "VM box has no harness antigravity") {
		t.Errorf("err = %v", err)
	}
}

func TestAResetKeepsTheSubscriptionsAsTheyAreNow(t *testing.T) {
	d, _ := subscriptionDaemon(t)
	r, _ := d.Store.Load(testOwner, "box")
	r.Template = &store.Template{Egress: cloneEgress(r.Egress)}
	if err := d.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, err := d.secretLogin(testOwner, api.SecretLoginParams{VM: "box", Harness: "claude", Value: "tok"}); err != nil {
		t.Fatal(err)
	}
	d.VMs.(*fakeRunner).running["id-box"] = false
	d.NewDisk = func(string, string, uint64) error { return nil }
	if _, err := d.reset(testOwner, api.VMParams{VM: "box"}); err != nil {
		t.Fatal(err)
	}
	r, _ = d.Store.Load(testOwner, "box")
	if e := r.Egress; !slices.Equal(e.Subscriptions, []string{"claude"}) || !slices.Equal(e.Secrets, []string{"box.claude.token"}) {
		t.Errorf("after reset %+v", e)
	}
}
