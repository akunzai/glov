package exec

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/internal/session"
	"github.com/akunzai/glov/guest/proto"
)

// testUser is an account the container image creates for these tests
// (useradd -m -G daemon gptest); without it, or without root, they skip.
const testUser = "gptest"

// asSignedIn points the session lookup at a record for testUser.
func asSignedIn(t *testing.T) *user.User {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("as: user tests need root to switch accounts")
	}
	u, err := user.Lookup(testUser)
	if err != nil {
		t.Skipf("account %q does not exist; create it with useradd -m -G daemon %s", testUser, testUser)
	}
	dir := t.TempDir()
	rec := fmt.Sprintf("UID=%s\nUSER=%s\nSTATE=active\nTYPE=x11\nCLASS=user\nSEAT=seat0\nDISPLAY=:7\n", u.Uid, u.Username)
	if err := os.WriteFile(filepath.Join(dir, "1"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	old := SessionsDir
	SessionsDir = dir
	t.Cleanup(func() { SessionsDir = old })
	return u
}

func TestAsUserRunsWithTheSignedInIdentity(t *testing.T) {
	u := asSignedIn(t)
	m, c := newManager()
	t.Setenv("SECRET_AGENT_VAR", "leak")
	pid := c.run(t, m, StartParams{As: "user", Argv: []string{"/bin/sh", "-c",
		`echo "$(id -u) $(id -g) $(id -G)"; echo "$HOME|$USER|$LOGNAME|$DISPLAY|$(pwd)|$FOO|$SECRET_AGENT_VAR"`},
		Env: map[string]string{"FOO": "bar"}})
	out, errOut, _, code := c.forPid(pid)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	ids := strings.Fields(lines[0])
	if ids[0] != u.Uid || ids[1] != u.Gid {
		t.Errorf("uid/gid = %s/%s, want %s/%s", ids[0], ids[1], u.Uid, u.Gid)
	}
	want, _ := u.GroupIds()
	if len(ids)-2 != len(want) {
		t.Errorf("groups = %v, want %v", ids[2:], want)
	}
	if exp := fmt.Sprintf("%s|%s|%s|:7|%s|bar|", u.HomeDir, u.Username, u.Username, u.HomeDir); lines[1] != exp {
		t.Errorf("env line = %q, want %q", lines[1], exp)
	}
}

func TestAsUserKillTakesTheTree(t *testing.T) {
	asSignedIn(t)
	m, c := newManager()
	res, err := m.Start(StartParams{As: "user", Argv: []string{"/bin/sh", "-c", "sleep 60 & echo $!; wait"}})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.exitChan(res.Pid)
	c.mu.Unlock()
	res.Sent(true)
	var grandchild int
	for deadline := time.Now().Add(5 * time.Second); grandchild == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		out, _, _, _ := c.forPid(res.Pid)
		grandchild, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	if grandchild == 0 {
		t.Fatal("never saw the grandchild pid")
	}
	if _, err := m.Kill(KillParams{Pid: res.Pid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no exec.exit after kill")
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(grandchild, 0) == nil && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if syscall.Kill(grandchild, 0) == nil {
		t.Fatal("grandchild survived the kill")
	}
	if _, _, _, code := c.forPid(res.Pid); code != 137 {
		t.Errorf("killed exit code = %d, want 137", code)
	}
}

func TestAsUserWithNobodySignedInIsError4(t *testing.T) {
	old := SessionsDir
	SessionsDir = t.TempDir()
	defer func() { SessionsDir = old }()
	m, _ := newManager()
	if _, err := m.Start(StartParams{As: "user", Argv: []string{"/bin/true"}}); errCode(err) != proto.CodeNoUser {
		t.Fatalf("err = %v, want code 4", err)
	}
}

func TestExplicitCwdOverridesTheUsersHome(t *testing.T) {
	asSignedIn(t)
	m, c := newManager()
	pid := c.run(t, m, StartParams{As: "user", Cwd: "/tmp", Argv: []string{"/bin/pwd"}})
	if out, _, _, _ := c.forPid(pid); string(out) != "/tmp\n" {
		t.Fatalf("cwd = %q", out)
	}
}

func TestUserEnvAddsTheSystemEnvironmentWithoutOverridingTheLogin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "environment")
	content := "PATH=\"/usr/local/bin:/snap/bin\"\n# a comment\n\nANTHROPIC_BASE_URL=http://127.0.0.1:3128/anthropic\nHOME=/elsewhere\nQUOTED='a b'\nnot a line\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := EnvironmentFile
	EnvironmentFile = file
	t.Cleanup(func() { EnvironmentFile = old })
	env := userEnv(&user.User{Uid: "4242", Username: "glov", HomeDir: "/home/glov"}, session.Session{})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Errorf("%s set twice", k)
		}
		got[k] = v
	}
	want := map[string]string{
		"HOME": "/home/glov", "PATH": defaultPath,
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:3128/anthropic", "QUOTED": "a b",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	EnvironmentFile = filepath.Join(t.TempDir(), "missing")
	if env := userEnv(&user.User{Uid: "4242", Username: "glov", HomeDir: "/home/glov"}, session.Session{}); len(env) == 0 {
		t.Error("a missing file leaves the login environment")
	}
}
