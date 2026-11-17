package exec

import (
	"io"
	"os"
	osexec "os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/akunzai/glov/guest/internal/egress"
	"github.com/akunzai/glov/guest/internal/session"
	"github.com/akunzai/glov/guest/proto"
)

// SessionsDir is where systemd-logind keeps its session records.
var SessionsDir = "/run/systemd/sessions"

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// EnvironmentFile is the system environment pam_env gives a login, where an
// isolated VM's harness profiles put their settings.
var EnvironmentFile = "/etc/environment"

// Identity is the account of the signed-in user and how to run as it.
type Identity struct {
	Session session.Session
	User    *user.User
	Cred    *syscall.Credential
	Env     []string
}

// SignedInUser resolves the signed-in user from the logind sessions in dir.
// It fails with protocol error 4 when nobody is signed in.
func SignedInUser(dir string) (*Identity, error) {
	s, ok := session.Active(dir)
	if !ok {
		return nil, proto.Errorf(proto.CodeNoUser, "no user is signed in")
	}
	u, err := user.LookupId(strconv.Itoa(s.UID))
	if err != nil {
		return nil, proto.Errorf(proto.CodeOS, "look up uid %d: %v", s.UID, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, proto.Errorf(proto.CodeOS, "bad gid %q for %s", u.Gid, u.Username)
	}
	groups := []uint32{}
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	return &Identity{
		Session: s,
		User:    u,
		Cred:    &syscall.Credential{Uid: uint32(s.UID), Gid: uint32(gid), Groups: groups},
		Env:     userEnv(u, s),
	}, nil
}

// userEnv is the environment of a login of u: nothing is inherited from the
// Guest Service. DISPLAY comes from the logind record, WAYLAND_DISPLAY from the
// compositor's socket in the runtime directory.
func userEnv(u *user.User, s session.Session) []string {
	runtime := "/run/user/" + u.Uid
	env := []string{
		"HOME=" + u.HomeDir,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"SHELL=" + loginShell(u.Username),
		"PATH=" + defaultPath,
	}
	if v := os.Getenv("LANG"); v != "" {
		env = append(env, "LANG="+v)
	}
	// An isolated VM's Guest Service sets these in its own environment.
	for k := range egress.Env() {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	if fi, err := os.Stat(runtime); err == nil && fi.IsDir() {
		env = append(env, "XDG_RUNTIME_DIR="+runtime)
		if _, err := os.Stat(runtime + "/bus"); err == nil {
			env = append(env, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtime+"/bus")
		}
		if s.Graphical() {
			if socks, _ := filepath.Glob(runtime + "/wayland-*[0-9]"); len(socks) > 0 {
				env = append(env, "WAYLAND_DISPLAY="+filepath.Base(socks[0]))
			}
		}
	}
	if s.Graphical() && s.Display != "" {
		env = append(env, "DISPLAY="+s.Display)
	}
	return append(env, systemEnv(env)...)
}

// systemEnv is EnvironmentFile as KEY=VALUE pairs (quotes around a value
// dropped), without the names set already: the login's own come first.
func systemEnv(set []string) []string {
	data, err := os.ReadFile(EnvironmentFile)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		seen[k] = true
	}
	var env []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.HasPrefix(k, "#") || strings.ContainsAny(k, " \t") || seen[k] {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		seen[k] = true
		env = append(env, k+"="+v)
	}
	return env
}

// loginShell is the shell field of the account's passwd entry, /bin/sh if unreadable.
func loginShell(name string) string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "/bin/sh"
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 7 && f[0] == name && f[6] != "" {
			return f[6]
		}
	}
	return "/bin/sh"
}

func startUser(p StartParams) (child, io.WriteCloser, error) {
	id, err := SignedInUser(SessionsDir)
	if err != nil {
		return nil, nil, err
	}
	return startAs(p, id)
}

func startAs(p StartParams, id *Identity) (child, io.WriteCloser, error) {
	cmd := osexec.Command(p.Argv[0], p.Argv[1:]...)
	cmd.Dir = p.Cwd
	if cmd.Dir == "" {
		cmd.Dir = id.User.HomeDir
	}
	cmd.Env = mergeEnv(id.Env, p.Env, false)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: id.Cred}
	return launch(cmd, p.Stdin)
}
