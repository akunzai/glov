package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheRootIsTheNearestFolderWithGit(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	os.MkdirAll(sub, 0o700)
	if root, ok := Root(sub); ok || root != sub {
		t.Errorf("no git: %s, %v", root, ok)
	}
	os.WriteFile(filepath.Join(dir, "a", ".git"), []byte("gitdir: x\n"), 0o600)
	if root, ok := Root(sub); !ok || root != filepath.Join(dir, "a") {
		t.Errorf("a worktree's .git file: %s, %v", root, ok)
	}
}

func TestVMNamesAreValidAndApartPerPath(t *testing.T) {
	a, b := VMName(`C:\src\My Project!`), VMName(`D:\src\My Project!`)
	if !strings.HasPrefix(a, "my-project-") || len(a) != len("my-project-")+6 || a == b {
		t.Errorf("names %s %s", a, b)
	}
	if n := VMName(`C:\src\___`); !strings.HasPrefix(n, "project-") {
		t.Errorf("no letters: %s", n)
	}
	if n := VMName(`C:\` + strings.Repeat("ab-", 20)); len(n) > 31 || strings.Contains(n, "--") {
		t.Errorf("long: %s", n)
	}
}

func TestSessionsAndTheirTmuxCommand(t *testing.T) {
	if s := Session("claude", []string{"claude", "claude-2"}, true); s != "claude-3" {
		t.Errorf("new session %s", s)
	}
	if s := Session("claude", []string{"claude"}, false); s != "claude" {
		t.Errorf("session %s", s)
	}
	got := TmuxCommand("claude", "claude", "/mnt/c/src/p q", []string{"claude", "-c", "it's"})
	want := `exec tmux -L claude new-session -A -s claude -c '/mnt/c/src/p q' -- claude -c 'it'\''s'`
	if got != want {
		t.Errorf("tmux %s", got)
	}
}

func TestARemoteNamesOneRepository(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/o/r.git":        "github.com/o/r",
		"https://user@GitLab.com:443/g/s/r": "gitlab.com/g/s/r",
		"ssh://git@github.com/o/r":          "github.com/o/r",
		"git@github.com:o/r.git":            "github.com/o/r",
		`C:\src\r`:                          "",
		"file:///c/src/r":                   "",
		"https://github.com/o/*":            "",
		"../r":                              "",
		"glov-px:/mnt/c/src/r":              "",
	} {
		p, ok := RepoOf(url)
		if got := p.Host + "/" + p.Path; (want == "") == ok || (ok && got != want) {
			t.Errorf("%s = %+v %v, want %q", url, p, ok, want)
		}
	}
}

func TestGlovsSSHConfigKeepsOneBlockPerVM(t *testing.T) {
	h := SSHHost{VM: "p-1", Key: `C:\k\id`, KnownHosts: `C:\k\kh`, Relay: `"C:\glov.exe" ssh-relay p-1`}
	text := WithSSHHost("", h)
	text = WithSSHHost(text, SSHHost{VM: "q-2", Key: "k", KnownHosts: "kh", Relay: "r"})
	text = WithSSHHost(text, h)
	if strings.Count(text, "Host glov-p-1\n") != 1 || strings.Count(text, "Host glov-q-2\n") != 1 || !strings.Contains(text, `IdentityFile "C:/k/id"`) ||
		!strings.Contains(text, "  ForwardAgent no\n") || !strings.Contains(text, "  ControlPath none\n") || !strings.Contains(text, "  EnableEscapeCommandline no\n") {
		t.Errorf("config:\n%s", text)
	}
	if got := WithoutSSHHost(text, "q-2"); strings.Contains(got, "q-2") || !strings.Contains(got, "Host glov-p-1") {
		t.Errorf("without q-2:\n%s", got)
	}
}

func TestTheIncludeGoesFirstOnce(t *testing.T) {
	path, home := `C:\Users\u\AppData\Local\glov\ssh_config`, `C:\Users\u`
	cfg := "Host gh\n  User git\n"
	if HasInclude(cfg, path, home) {
		t.Error("an include where there is none")
	}
	cfg = WithInclude(cfg, path, home)
	if !HasInclude(cfg, path, home) || strings.SplitN(cfg, "\n", 3)[1] != `Include "~/AppData/Local/glov/ssh_config"` || !strings.HasSuffix(cfg, "Host gh\n  User git\n") {
		t.Errorf("config:\n%s", cfg)
	}
	if !HasInclude(`include ~/appdata/local/glov/SSH_CONFIG`, path, home) {
		t.Error("an include of another case")
	}
	// Git for Windows' ssh reads C:/... as a path in ~/.ssh.
	if HasInclude(`Include "C:/Users/u/AppData/Local/glov/ssh_config"`, path, home) {
		t.Error("an include of the full path counts")
	}
	if got := IncludeLine(`D:\glov\ssh_config`, home); got != `Include "D:/glov/ssh_config"` {
		t.Errorf("outside home: %s", got)
	}
	if got := IncludeLine(`C:\Users\u2\glov\ssh_config`, home); got != `Include "C:/Users/u2/glov/ssh_config"` {
		t.Errorf("in a sibling of home: %s", got)
	}
}

func TestRemotesAreConfirmedPerProject(t *testing.T) {
	r := Remotes{Path: filepath.Join(t.TempDir(), "remotes.json")}
	if c, err := r.Confirmed(`C:\p`); err != nil || len(c) != 0 {
		t.Fatalf("none yet: %v, %v", c, err)
	}
	r.Confirm(`C:\p`, []string{"github.com/o/b", "github.com/o/a"})
	r.Confirm(`c:\P`, []string{"github.com/o/a"})
	if c, _ := r.Confirmed(`C:\P`); len(c) != 2 || c[0] != "github.com/o/a" {
		t.Errorf("confirmed %v", c)
	}
	answer := func(s string) func() string { return func() string { return s } }
	for _, c := range []struct {
		anyNew, terminal, flag bool
		answer                 string
		want                   bool
	}{
		{false, true, false, "", true}, {true, true, false, "", false}, {true, true, false, "y", true},
		{false, true, false, "n", false}, {false, false, false, "", false}, {true, false, true, "", true},
	} {
		if got := ApplyRemotes(c.anyNew, c.terminal, c.flag, answer(c.answer)); got != c.want {
			t.Errorf("%+v = %v", c, got)
		}
	}
}

func TestASignInWrittenIntoTheOriginStaysOnTheHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://me:ghp_x@github.com/o/r.git":       "https://github.com/o/r.git",
		"https://oauth2@gitlab.example.com/o/r.git": "https://gitlab.example.com/o/r.git",
		"https://github.com/o/r.git":                "https://github.com/o/r.git",
		"https://ghp_x@github.com/o/r.git":          "https://github.com/o/r.git",
		"ssh://git@github.com/o/r.git":              "ssh://git@github.com/o/r.git",
		"git@github.com:o/r.git":                    "git@github.com:o/r.git",
		"":                                          "",
	} {
		if got := WithoutCredentials(in); got != want {
			t.Errorf("WithoutCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}
