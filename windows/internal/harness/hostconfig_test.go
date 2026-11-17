package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHostConfigIsStagedWithoutCredentialsOrWhatIsMissing(t *testing.T) {
	home, staging := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "be brief")
	write(t, filepath.Join(home, ".claude", "skills", "a", "SKILL.md"), "skill")
	write(t, filepath.Join(home, ".claude", "skills", "a", ".env"), "KEY=x")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "SKILL.md"), "linked")
	if err := os.Symlink(outside, filepath.Join(home, ".claude", "skills", "b")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := os.Symlink(filepath.Join(home, "gone"), filepath.Join(home, ".claude", "skills", "c")); err != nil {
		t.Fatal(err)
	}
	copies := []HostCopy{{".claude/CLAUDE.md", ".claude/CLAUDE.md"}, {".claude/skills", ".claude/skills"}, {".claude/agents", ".claude/agents"}}
	rep, err := StageHostConfig(copies, home, staging)
	if err != nil {
		t.Fatal(err)
	}
	want := HostConfigReport{
		Copied:  []Copied{{".claude/CLAUDE.md", 1, 8}, {".claude/skills", 2, 11}},
		Skipped: []string{"~/.claude/skills/a/.env (credentials)", "~/.claude/skills/c (broken link)"},
	}
	if !reflect.DeepEqual(rep, want) {
		t.Errorf("report = %+v, want %+v", rep, want)
	}
	if b, err := os.ReadFile(filepath.Join(staging, ".claude", "skills", "b", "SKILL.md")); err != nil || string(b) != "linked" {
		t.Errorf("the linked skill = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(staging, ".claude", "skills", "a", ".env")); err == nil {
		t.Error("a credentials file was staged")
	}
}

func TestHostConfigLeavesOutALinkCycleAndStopsPastItsLimit(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "cfg", "a.md"), "a")
	if err := os.Symlink(filepath.Join(home, "cfg"), filepath.Join(home, "cfg", "loop")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	rep, err := StageHostConfig([]HostCopy{{"cfg", "cfg"}}, home, t.TempDir())
	if err != nil || len(rep.Skipped) != 1 || rep.Skipped[0] != "~/cfg/loop (link cycle)" {
		t.Errorf("report = %+v, %v", rep, err)
	}
	big := t.TempDir()
	write(t, filepath.Join(big, "cfg", "huge"), strings.Repeat("x", MaxHostConfigBytes+1))
	if _, err := StageHostConfig([]HostCopy{{"cfg", "cfg"}}, big, t.TempDir()); err == nil || !strings.Contains(err.Error(), "--no-host-config") {
		t.Errorf("an oversized copy = %v", err)
	}
}

func TestEveryProfileCopiesNoCredentialsAndLinksItsRecords(t *testing.T) {
	for _, p := range All {
		for _, c := range p.HostConfig {
			for _, part := range strings.Split(c.Host+"/"+c.Guest, "/") {
				for _, n := range CredentialNames {
					if part == n || part == ".." || part == "" {
						t.Errorf("%s copies %+v", p.Name, c)
					}
				}
			}
		}
		for _, dir := range p.Sessions {
			if !strings.HasPrefix(SessionsTarget(p.Name, dir), SessionsGuestPath+"/"+p.Name+"/") {
				t.Errorf("%s records %s land at %s", p.Name, dir, SessionsTarget(p.Name, dir))
			}
		}
	}
}
