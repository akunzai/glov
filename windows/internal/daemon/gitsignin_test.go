package daemon

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/gitsignin"
	"github.com/akunzai/glov/windows/internal/secret"
	"github.com/akunzai/glov/windows/internal/store"
)

// gitDaemon is a glovd with running isolated VM box and git secret gh
// (forge github), whose guest's /etc/gitconfig holds the owner's lines.
func gitDaemon(t *testing.T) (*Daemon, *fakeRunner) {
	t.Helper()
	st := store.Store{Root: t.TempDir()}
	if err := st.Create(store.Record{Name: "box", Owner: testOwner, Isolated: true, ID: "id-box", Egress: &store.Egress{Allow: []string{}}}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	runner.running["id-box"] = true
	owner := base64.StdEncoding.EncodeToString([]byte("[core]\n\teditor = vim\n"))
	runner.execOut = func(argv []string) (string, int) {
		if slices.Contains(argv, gitsignin.GitConfigPath) && len(argv) == 5 {
			return owner[:10] + "\n" + owner[10:] + "\n", 0
		}
		return "changed\n", 0
	}
	d := &Daemon{Owner: testOwner, Store: st, VMs: runner, Secrets: &secret.Store{Dir: t.TempDir(), Values: &secret.Memory{}}}
	if _, err := d.secretSet(testOwner, api.SecretParams{Name: "gh", Hosts: []string{"github.com", "api.github.com"}, Value: "tok-REAL", Forge: "github"}, false); err != nil {
		t.Fatal(err)
	}
	return d, runner
}

// files is what the last sync's two writes put, by path: base64 decoded,
// "-" for a removal.
func files(t *testing.T, r *fakeRunner) map[string]string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	writes := 0
	for _, e := range slices.Backward(r.execs) {
		args := e.Argv
		if i := max(slices.Index(args, putScript), slices.Index(args, userPutScript)); i >= 0 {
			writes++
			args = args[i+2:]
			for ; len(args) >= 3; args = args[3:] {
				v := args[2]
				if v != "-" {
					b, err := base64.StdEncoding.DecodeString(v)
					if err != nil {
						t.Fatal(err)
					}
					v = string(b)
				}
				out[args[0]] = v
			}
			if writes == 2 {
				return out
			}
		}
	}
	t.Fatalf("no sync in %d commands", len(r.execs))
	return nil
}

func TestAGrantSetsTheGuestUpAndARevokeUndoesIt(t *testing.T) {
	d, runner := gitDaemon(t)
	if _, err := d.secretGrant(testOwner, api.SecretGrantParams{VM: "box", Names: []string{"gh"}}, true); err != nil {
		t.Fatal(err)
	}
	f := files(t, runner)
	if f[gitsignin.SignInsPath] != "github.com github gh\n" || f[gitsignin.HelperPath] != gitsignin.Helper {
		t.Errorf("system files %q", f)
	}
	if c := f[gitsignin.GitConfigPath]; !strings.HasPrefix(c, "[core]\n\teditor = vim\n") || !strings.Contains(c, "insteadOf = git@github.com:") {
		t.Errorf("gitconfig %q", c)
	}
	if h := f[gitsignin.GHHostsPath]; !strings.Contains(h, "oauth_token: glov-secret:gh") || strings.Contains(h, "REAL") {
		t.Errorf("gh hosts %q", h)
	}
	if f[gitsignin.GlabConfigPath] != "-" {
		t.Errorf("glab config %q", f[gitsignin.GlabConfigPath])
	}
	runner.mu.Lock()
	user := runner.execs[len(runner.execs)-1].Argv
	runner.mu.Unlock()
	if len(user) < 6 || !strings.Contains(user[2], "runuser -u") || user[4] != "glov" || user[5] != userPutScript {
		t.Errorf("the user's files are written by %q", user[:min(6, len(user))])
	}
	if _, err := d.secretGrant(testOwner, api.SecretGrantParams{VM: "box", Names: []string{"gh"}}, false); err != nil {
		t.Fatal(err)
	}
	f = files(t, runner)
	if _, ok := f[gitsignin.HelperPath]; ok || f[gitsignin.SignInsPath] != "" || f[gitsignin.GHHostsPath] != "-" {
		t.Errorf("after revoke %q", f)
	}
	// The owner's lines come back as they were; the fake guest's file never
	// held the block.
	if _, ok := f[gitsignin.GitConfigPath]; ok {
		t.Errorf("gitconfig rewritten without a change: %q", f[gitsignin.GitConfigPath])
	}
}

func TestAFailedGitSyncIsReported(t *testing.T) {
	d, runner := gitDaemon(t)
	runner.execOut = func([]string) (string, int) { return "sh: base64: not found\n", 127 }
	_, err := d.secretGrant(testOwner, api.SecretGrantParams{VM: "box", Names: []string{"gh"}}, true)
	if err == nil || !strings.Contains(err.Error(), "saved, but box: cannot set up git sign-ins: reading /etc/gitconfig exited 127") {
		t.Errorf("err = %v", err)
	}
}

func TestForgeMustBeGitHubOrGitLab(t *testing.T) {
	d, _ := gitDaemon(t)
	_, err := d.secretSet(testOwner, api.SecretParams{Name: "x", Hosts: []string{"example.com"}, Value: "v", Forge: "gitea"}, false)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams || e.Message != "forge gitea: use github or gitlab" {
		t.Errorf("err = %v", err)
	}
	list, _ := d.secretList()
	if i := slices.IndexFunc(list.Secrets, func(s api.Secret) bool { return s.Name == "gh" }); i < 0 || list.Secrets[i].Forge != "github" {
		t.Errorf("list %+v", list.Secrets)
	}
}

func TestSecretReposChangesAndAnswersTheVMsRepositories(t *testing.T) {
	d, _ := gitDaemon(t)
	r, err := d.secretRepos(testOwner, api.SecretReposParams{VM: "box"})
	if err != nil || r.VM != "box" || r.Repos == nil || len(r.Repos) != 0 {
		t.Fatalf("%+v, %v", r, err)
	}
	r, err = d.secretRepos(testOwner, api.SecretReposParams{VM: "box", Add: []string{"https://GitHub.com/Owner/App.git", "gitlab.example.com/team/*", "github.com/owner/app"}})
	if err != nil || !slices.Equal(r.Repos, []string{"github.com/owner/app", "gitlab.example.com/team/*"}) {
		t.Fatalf("%+v, %v", r, err)
	}
	_, err = d.secretRepos(testOwner, api.SecretReposParams{VM: "box", Add: []string{"github.com/owner"}})
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams ||
		e.Message != "repository github.com/owner: give <host>/<owner>/<repo> or <host>/<group>/*" {
		t.Errorf("bad pattern err = %v", err)
	}
	if _, err := d.secretRepos(testOwner, api.SecretReposParams{VM: "box", Remove: []string{"github.com/owner/other"}}); err == nil ||
		err.Error() != "VM box has no repository github.com/owner/other" {
		t.Errorf("remove a missing one err = %v", err)
	}
	r, err = d.secretRepos(testOwner, api.SecretReposParams{VM: "box", Remove: []string{"github.com/owner/app", "gitlab.example.com/team/*"}})
	if err != nil || len(r.Repos) != 0 {
		t.Fatalf("%+v, %v", r, err)
	}
	if rec, _ := d.Store.Load(testOwner, "box"); rec.Egress.Repos != nil {
		t.Errorf("vm.json keeps %q", rec.Egress.Repos)
	}
	if err := d.Store.Create(store.Record{Name: "plain", Owner: testOwner}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.secretRepos(testOwner, api.SecretReposParams{VM: "plain"}); err == nil || !strings.Contains(err.Error(), "is not isolated") {
		t.Errorf("a VM that is not isolated: %v", err)
	}
}

// TestThePutScriptWritesOnlyWhatDiffers runs putScript in a real sh.
func TestThePutScriptWritesOnlyWhatDiffers(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("needs a POSIX sh on PATH")
	}
	dir := t.TempDir()
	put := func(args ...string) string {
		cmd := exec.Command(sh, append([]string{"-c", putScript, "sh"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	if out := put("a/b.yml", "600", b64("token: glov-secret:gh\n"), "empty", "644", ""); out != "changed" {
		t.Errorf("first write printed %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a", "b.yml")); string(data) != "token: glov-secret:gh\n" {
		t.Errorf("wrote %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "empty")); err == nil {
		t.Error("empty content created a file")
	}
	if out := put("a/b.yml", "600", b64("token: glov-secret:gh\n")); out != "" {
		t.Errorf("an unchanged file printed %q", out)
	}
	os.WriteFile(filepath.Join(dir, "mine.yml"), []byte("token: my own\n"), 0o600)
	if out := put("a/b.yml", "600", "-", "mine.yml", "600", "-"); out != "changed" {
		t.Errorf("removal printed %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "b.yml")); err == nil {
		t.Error("glov's file is still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.yml")); err != nil {
		t.Error("a file without a placeholder was removed")
	}
}
