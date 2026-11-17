package gitsignin

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/egress"
)

var (
	gh = Entry{Name: "gh", Forge: egress.GitHub, Hosts: []string{"github.com", "api.github.com", "uploads.github.com"}}
	gl = Entry{Name: "gl", Forge: egress.GitLab, Hosts: []string{"gitlab.example.com:443", "*.example.com", "10.0.0.1", "gitlab.example.com:8443"}}
)

func TestGitHostsFoldAPIHostsAndSkipWhatNamesNoGitHost(t *testing.T) {
	if got := gh.GitHosts(); !slices.Equal(got, []string{"github.com"}) {
		t.Errorf("gh: %q", got)
	}
	if got := gl.GitHosts(); !slices.Equal(got, []string{"gitlab.example.com"}) {
		t.Errorf("gl: %q", got)
	}
	if got := SignIns([]Entry{gl, gh}); got != "github.com github gh\ngitlab.example.com gitlab gl\n" {
		t.Errorf("sign-ins %q", got)
	}
}

func TestGitConfigKeepsTheOwnersLinesAndReplacesItsBlock(t *testing.T) {
	owner := "[core]\n\teditor = vim\n"
	once := GitConfig(owner, []Entry{gh, gl})
	if !strings.HasPrefix(once, owner) || !strings.Contains(once, "[credential]\n\thelper = glov\n") ||
		!strings.Contains(once, "[url \"https://gitlab.example.com/\"]\n\tinsteadOf = git@gitlab.example.com:\n\tinsteadOf = ssh://git@gitlab.example.com/\n") {
		t.Fatalf("%q", once)
	}
	if GitConfig(once, []Entry{gh, gl}) != once {
		t.Error("not idempotent")
	}
	if strings.Contains(GitConfig(once, []Entry{gh}), "gitlab") {
		t.Error("gitlab left in")
	}
	if got := GitConfig(once, nil); got != owner {
		t.Errorf("without entries %q", got)
	}
	if got := GitConfig("", nil); got != "" {
		t.Errorf("empty %q", got)
	}
}

func TestCLIConfigsHoldPlaceholdersOnly(t *testing.T) {
	hosts, ok := GHHosts([]Entry{gh, gl})
	if !ok || !strings.Contains(hosts, "github.com:\n") || !strings.Contains(hosts, "oauth_token: glov-secret:gh") || strings.Contains(hosts, "gitlab") {
		t.Errorf("gh %q", hosts)
	}
	glab, ok := GlabConfig([]Entry{gh, gl})
	if !ok || !strings.Contains(glab, "host: gitlab.example.com\nhosts:\n    gitlab.example.com:\n") || !strings.Contains(glab, "token: glov-secret:gl") {
		t.Errorf("glab %q", glab)
	}
	m := regexp.MustCompile(`(?m)^        oauth2_expiry_date: (.+)$`).FindStringSubmatch(glab)
	if m == nil {
		t.Fatalf("glab config has no oauth2_expiry_date: %q", glab)
	}
	if exp, err := time.Parse("02 Jan 06 15:04 MST", m[1]); err != nil || !exp.After(time.Now().AddDate(10, 0, 0)) {
		t.Errorf("oauth2_expiry_date %q: %v, want a parseable date over ten years out", m[1], err)
	}
	if _, ok := GHHosts([]Entry{gl}); ok {
		t.Error("gh hosts without a GitHub entry")
	}
	if _, ok := GlabConfig([]Entry{gh}); ok {
		t.Error("glab config without a GitLab entry")
	}
}

func TestTheHelperAnswersOnlyHTTPSHostsItKnows(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("needs a POSIX sh on PATH")
	}
	dir := t.TempDir()
	signIns := filepath.ToSlash(filepath.Join(dir, "sign-ins"))
	if err := os.WriteFile(signIns, []byte(SignIns([]Entry{gh, gl})), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "helper")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(Helper, SignInsPath, signIns)), 0o755); err != nil {
		t.Fatal(err)
	}
	ask := func(input string, action string) string {
		cmd := exec.Command(sh, script, action)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		return string(out)
	}
	for _, c := range []struct{ input, action, want string }{
		{"protocol=https\nhost=gitlab.example.com\npath=team/x.git\n\n", "get", "username=oauth2\npassword=glov-secret:gl\n"},
		{"protocol=https\nhost=github.com\n\n", "get", "username=x-access-token\npassword=glov-secret:gh\n"},
		{"protocol=https\nhost=example.org\n\n", "get", ""},
		{"protocol=http\nhost=github.com\n\n", "get", ""},
		{"protocol=https\nhost=github.com\n\n", "store", ""},
	} {
		if got := ask(c.input, c.action); got != c.want {
			t.Errorf("%q %s: %q, want %q", c.input, c.action, got, c.want)
		}
	}
}
