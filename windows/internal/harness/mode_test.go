package harness

import (
	"encoding/json/jsontext"
	"slices"
	"strings"
	"testing"
)

func named(t *testing.T, name string) Profile {
	t.Helper()
	p, err := Named(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEveryProfileHasAValidSubscription holds the profiles' subscriptions to
// macOS's HarnessProfile.secretProblems rules.
func TestEveryProfileHasAValidSubscription(t *testing.T) {
	for _, p := range All {
		sub := p.Subscription
		if sub == nil {
			t.Errorf("%s has no subscription", p.Name)
			continue
		}
		answered := map[string]bool{}
		for _, e := range sub.TokenEndpoints {
			for _, s := range e.Answer {
				answered[s] = true
			}
			if !slices.Contains(p.SignedIn("v").Runtime, e.Host) {
				t.Errorf("%s: token endpoint %s is not in the signed-in runtime", p.Name, e.Host)
			}
		}
		for _, s := range sub.Secrets {
			if len(s.Hosts) == 0 {
				t.Errorf("%s: subscription secret %s names no hosts", p.Name, s.Name)
			}
			if s.Env == "" && s.File == "" && !answered[s.Name] {
				t.Errorf("%s: subscription secret %s reaches the guest nowhere", p.Name, s.Name)
			}
			for _, h := range s.Hosts {
				if !slices.Contains(p.SignedIn("v").Runtime, h) {
					t.Errorf("%s: subscription secret %s goes to %s, not in the signed-in runtime", p.Name, s.Name, h)
				}
			}
		}
		for s := range answered {
			if !slices.ContainsFunc(sub.Secrets, func(x Secret) bool { return x.Name == s }) {
				t.Errorf("%s: a token endpoint answers into %s, which is not one of its secrets", p.Name, s)
			}
		}
		if (sub.SignIn == nil) == (sub.PasteHint == "") {
			t.Errorf("%s: give exactly one of signIn and pasteHint", p.Name)
		}
		if sub.SignIn == nil && len(sub.Secrets) != 1 {
			t.Errorf("%s: a pasted sign-in stores exactly one secret", p.Name)
		}
		for _, s := range sub.Settings {
			i := slices.IndexFunc(p.Files, func(f File) bool { return f.Path == s.Path })
			if i < 0 {
				t.Errorf("%s: subscription setting %s is not one of the profile's files", p.Name, s.Path)
				continue
			}
			if s.IsTOML() && (len(s.Key) != 1 || p.Files[i].Text == nil) {
				t.Errorf("%s: subscription setting %s: a TOML setting is one top-level key of a text file", p.Name, s.Path)
			}
			if !s.IsTOML() && (len(s.Key) == 0 || p.Files[i].Text != nil) {
				t.Errorf("%s: subscription setting %s: not a JSON key path", p.Name, s.Path)
			}
		}
	}
}

func TestSignedInSwapsTheAPIKeyForTheVMsOwnSecrets(t *testing.T) {
	c := named(t, "claude").SignedIn("box").GuestEnvironment()
	if _, ok := c["ANTHROPIC_API_KEY"]; ok || c["CLAUDE_CODE_OAUTH_TOKEN"] != "glov-secret:box.claude.token" || c["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Errorf("claude signed in: %v", c)
	}
	x := named(t, "codex").SignedIn("box")
	if _, ok := x.GuestEnvironment()["CODEX_API_KEY"]; ok {
		t.Error("codex signed in keeps CODEX_API_KEY")
	}
	var names []string
	for _, s := range x.Secrets {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"box.codex.access", "box.codex.refresh"}) || !slices.Contains(x.Runtime, "chatgpt.com") || !slices.Contains(x.Runtime, "api.openai.com") {
		t.Errorf("codex signed in: %v, %v", names, x.Runtime)
	}
	if named(t, "codex").Secrets[0].Name == "box.codex.access" {
		t.Error("SignedIn changed the profile it was called on")
	}
}

func TestTheEnvironmentKeepsOtherLinesAndHoldsOneModesVariables(t *testing.T) {
	ps := []Profile{named(t, "claude"), named(t, "codex")}
	base := "PATH=/usr/bin\nHTTPS_PROXY=http://127.0.0.1:3128\nANTHROPIC_API_KEY=glov-secret:anthropic\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\nCODEX_API_KEY=glov-secret:openai\n"
	on := Environment(base, ps, []string{"claude"}, "box")
	if !strings.HasPrefix(on, "PATH=/usr/bin\nHTTPS_PROXY=http://127.0.0.1:3128\n") || strings.Contains(on, "ANTHROPIC_API_KEY") ||
		!strings.Contains(on, "CLAUDE_CODE_OAUTH_TOKEN=glov-secret:box.claude.token\n") || !strings.Contains(on, "CODEX_API_KEY=glov-secret:openai\n") {
		t.Errorf("on: %q", on)
	}
	off := Environment(on, ps, nil, "box")
	if strings.Contains(off, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(off, "ANTHROPIC_API_KEY=glov-secret:anthropic\n") {
		t.Errorf("off: %q", off)
	}
	if again := Environment(off, ps, nil, "box"); again != off {
		t.Errorf("applying twice changed %q to %q", off, again)
	}
}

func TestOnlyTheDifferingSettingsChangeSoTheOwnersEditsStay(t *testing.T) {
	codex, agy := named(t, "codex"), named(t, "antigravity")
	i := slices.IndexFunc(codex.Files, func(f File) bool { return f.Path == ".codex/config.toml" })
	edited := strings.Replace(*codex.Files[i].Text, `approval_policy = "on-request"`, `approval_policy = "never"`, 1)
	on := Edits([]Profile{codex}, []string{"codex"}, "box")
	if len(on) != 1 || on[0].Path != ".codex/config.toml" || !slices.Equal(on[0].Key, []string{"model_provider"}) || string(on[0].Value) != `"glov-chatgpt"` {
		t.Fatalf("codex edits %+v", on)
	}
	signedIn, ok := Apply(on, edited, ".codex/config.toml")
	if !ok || !strings.Contains(signedIn, "model_provider = \"glov-chatgpt\"\n") || !strings.Contains(signedIn, `approval_policy = "never"`) ||
		!strings.Contains(signedIn, "[model_providers.glov]\n") || !strings.Contains(signedIn, "[model_providers.glov-chatgpt]\n") {
		t.Fatalf("signed in: %q", signedIn)
	}
	if back, _ := Apply(Edits([]Profile{codex}, nil, "box"), signedIn, ".codex/config.toml"); back != edited {
		t.Errorf("signed out: %q, want %q", back, edited)
	}

	const path = ".gemini/antigravity-cli/settings.json"
	settings := `{"agentMode": "plan", "modelProvider": "gemini", "toolPermission": "always-proceed"}`
	agyOn, ok := Apply(Edits([]Profile{agy}, []string{"antigravity"}, "box"), settings, path)
	if !ok || strings.Contains(agyOn, "modelProvider") || !strings.Contains(agyOn, `"agentMode": "plan"`) {
		t.Errorf("antigravity on: %q", agyOn)
	}
	agyOff, ok := Apply(Edits([]Profile{agy}, nil, "box"), agyOn, path)
	if !ok || !strings.Contains(agyOff, `"modelProvider": "gemini"`) || !strings.Contains(agyOff, `"agentMode": "plan"`) {
		t.Errorf("antigravity off: %q", agyOff)
	}
	// A file the owner broke is left alone rather than replaced.
	if _, ok := Apply(Edits([]Profile{agy}, nil, "box"), "{oops", path); ok {
		t.Error("a broken file was rewritten")
	}
	if got, ok := Apply([]Edit{{Path: "x.json", Key: []string{"a", "b"}, Value: jsontext.Value(`"<v>"`)}}, "", "x.json"); !ok || got != "{\n  \"a\": {\n    \"b\": \"<v>\"\n  }\n}\n" {
		t.Errorf("a new file: %q", got)
	}
}

func TestTOMLKeysAreSetAddedAndRemovedAtTheTopLevelOnly(t *testing.T) {
	text := "# c\nmodel = \"a\"\n\n[t]\nmodel = \"inner\"\n"
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		key   string
		value *string
		want  string
	}{
		{"model", str("b"), "# c\nmodel = \"b\"\n\n[t]\nmodel = \"inner\"\n"},
		{"model", nil, "# c\n\n[t]\nmodel = \"inner\"\n"},
		{"other", str(`x"y`), "# c\nmodel = \"a\"\nother = \"x\\\"y\"\n\n[t]\nmodel = \"inner\"\n"},
	} {
		if got := SettingTOML(text, c.key, c.value); got != c.want {
			t.Errorf("%s: %q, want %q", c.key, got, c.want)
		}
	}
	if v, ok := TOMLValue(text, "model"); !ok || v != "a" {
		t.Errorf("TOMLValue = %q, %v", v, ok)
	}
}

func TestTheMarkerListsTheSignedInHarnesses(t *testing.T) {
	if m := Marker([]string{"codex", "claude"}); m != "claude\ncodex\n" {
		t.Errorf("marker %q", m)
	}
	if got := ParseMarker("claude\n\ncodex\n"); !slices.Equal(got, []string{"claude", "codex"}) {
		t.Errorf("parsed %q", got)
	}
}
