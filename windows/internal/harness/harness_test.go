package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// macOS is where macOS's profiles live in the repository.
var macOS = filepath.Join("..", "..", "..", "Sources", "GlovCore", "Harnesses")

func TestTheProfilesAreMacOSsByteForByte(t *testing.T) {
	theirs, err := filepath.Glob(filepath.Join(macOS, "*.json"))
	if err != nil || len(theirs) == 0 {
		t.Fatalf("no macOS profiles in %s: %v", macOS, err)
	}
	ours, _ := files.ReadDir("profiles")
	if len(ours) != len(theirs) {
		t.Errorf("%d profiles here, %d on macOS", len(ours), len(theirs))
	}
	for _, path := range theirs {
		want, _ := os.ReadFile(path)
		got, err := files.ReadFile("profiles/" + filepath.Base(path))
		// A checkout may turn LF into CRLF in either copy.
		norm := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
		if err != nil || !bytes.Equal(norm(got), norm(want)) {
			t.Errorf("%s differs from macOS's: copy it to windows/internal/harness/profiles (%v)", filepath.Base(path), err)
		}
	}
}

func TestProfilesNameTheirSecretsAndHosts(t *testing.T) {
	if Names() != "antigravity, claude or codex" {
		t.Errorf("Names = %q", Names())
	}
	for _, p := range All {
		if len(p.Secrets) == 0 || p.Command == "" || p.InstallScript == "" {
			t.Errorf("%s: %+v", p.Name, p)
		}
		for _, s := range p.Secrets {
			if len(s.Hosts) == 0 || (s.Env == "") == (s.File == "") || s.Delivery != Placeholder && s.Delivery != Tmpfs {
				t.Errorf("%s: secret %+v", p.Name, s)
			}
		}
		for _, f := range p.GuestFiles() {
			if strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") || !strings.HasSuffix(f.Content(), "\n") {
				t.Errorf("%s: file %q", p.Name, f.Path)
			}
		}
	}
	claude, _ := Named("claude")
	if env := claude.GuestEnvironment(); env["ANTHROPIC_API_KEY"] != "glov-secret:anthropic" || env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Errorf("claude environment = %v", env)
	}
	if _, err := Named("aider"); err == nil || !strings.Contains(err.Error(), "use antigravity, claude or codex") {
		t.Errorf("Named(aider) err = %v", err)
	}
	if ps, err := Resolve([]string{"codex", "claude", "codex"}); err != nil || len(ps) != 2 || ps[0].Name != "codex" {
		t.Errorf("Resolve = %v, %v", ps, err)
	}
}

func TestSecretsGoWhereTheirDeliverySays(t *testing.T) {
	p := Profile{
		Name:  "h",
		Files: []File{{Path: ".h/config.json", JSON: []byte(`{"keep":1}`)}},
		Secrets: []Secret{
			{Name: "api", Delivery: Placeholder, File: ".h/config.json", JSON: []string{"auth", "key"}, Hosts: []string{"api.example.com"}},
			{Name: "new", Delivery: Placeholder, File: ".h/new.json", JSON: []string{"k"}, Hosts: []string{"api.example.com"}},
			{Name: "raw", Delivery: Tmpfs, File: ".h/token", Hosts: []string{"auth.example.com"}},
			{Name: "env", Delivery: Tmpfs, Env: "H_TOKEN_FILE", Hosts: []string{"auth.example.com"}},
		},
	}
	files := p.GuestFiles()
	if len(files) != 2 || files[0].Content() != "{\n  \"auth\": {\n    \"key\": \"glov-secret:api\"\n  },\n  \"keep\": 1\n}\n" ||
		files[1].Path != ".h/new.json" || !strings.Contains(files[1].Content(), `"k": "glov-secret:new"`) {
		t.Errorf("files %v", files)
	}
	if links := p.SecretLinks(); !slices.Equal(links, []SecretLink{{".h/token", "/run/glov/secrets/raw"}}) {
		t.Errorf("links %v", links)
	}
	if env := p.GuestEnvironment(); env["H_TOKEN_FILE"] != "/run/glov/secrets/env" {
		t.Errorf("environment %v", env)
	}
	needs := p.SecretNeeds("box", []string{"api"})
	if len(needs) != 4 || needs[0] != "harness h: secret api (a placeholder in ~/.h/config.json at auth.key, sent to api.example.com) is defined, not granted: glov secret grant box api" ||
		needs[2] != "harness h: secret raw (the value in /run/glov/secrets/raw, linked from ~/.h/token, sent to auth.example.com) is not defined or granted: glov secret set raw --host auth.example.com, then glov secret grant box raw" {
		t.Errorf("needs %q", needs)
	}
	if n := p.SecretNeeds("box", nil); !strings.HasSuffix(n[0], "is not granted: glov secret grant box api (define it first if glov secret list lacks it: glov secret set api --host api.example.com)") {
		t.Errorf("needs without a list %q", n[0])
	}
}
