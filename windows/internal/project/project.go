// Package project is glov claude, codex and agy on a project: the project
// a directory belongs to, its VM's name, the harness's tmux session there,
// the repositories its remotes name, and the SSH host git reaches the
// guest's repository through.
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/akunzai/glov/windows/internal/egress"
)

// Root is the git root of dir: the nearest folder holding .git (a
// directory, or a worktree's file), and true; else dir itself and false.
func Root(dir string) (string, bool) {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir, false
		}
		d = parent
	}
}

// VMName is the project's VM: its folder's name made a valid VM name, and a
// hash of the whole path, so two folders of one name get two VMs (as on
// macOS).
func VMName(project string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(winBase(project)) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	base := b.String()
	for strings.Contains(base, "--") {
		base = strings.ReplaceAll(base, "--", "-")
	}
	base = strings.Trim(base, "-")
	if len(base) > 24 {
		base = strings.TrimRight(base[:24], "-")
	}
	if base == "" {
		base = "project"
	}
	sum := sha256.Sum256([]byte(project))
	return base + "-" + hex.EncodeToString(sum[:3])
}

// Session is the tmux session to attach: the harness's own, or with fresh
// the first of <harness>-2, <harness>-3... not in existing.
func Session(harness string, existing []string, fresh bool) string {
	if !fresh {
		return harness
	}
	for n := 2; ; n++ {
		if s := harness + "-" + strconv.Itoa(n); !slices.Contains(existing, s) {
			return s
		}
	}
}

// TmuxCommand is the ssh session's remote command: attach session, or start
// it running argv in dir. Each harness has a tmux server (-L) of its own,
// which takes the environment of the sign-in that starts it.
func TmuxCommand(server, session, dir string, argv []string) string {
	words := append([]string{"exec", "tmux", "-L", server, "new-session", "-A", "-s", session, "-c", dir, "--"}, argv...)
	for i, w := range words {
		words[i] = ShellQuote(w)
	}
	return strings.Join(words, " ")
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@%+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RepoOf is the repository a remote URL names (https://host/o/r.git,
// ssh://git@host/o/r, git@host:o/r.git) as a RepoPattern's text, false for
// a local path, a wildcard or anything else.
func RepoOf(url string) (egress.RepoPattern, bool) {
	var rest string
	if scheme, after, ok := strings.Cut(url, "://"); ok {
		if !slices.Contains([]string{"https", "http", "ssh", "git+ssh"}, strings.ToLower(scheme)) {
			return egress.RepoPattern{}, false
		}
		authority, p, ok := strings.Cut(after, "/")
		if !ok {
			return egress.RepoPattern{}, false
		}
		if i := strings.LastIndexByte(authority, '@'); i >= 0 {
			authority = authority[i+1:]
		}
		if i := strings.LastIndexByte(authority, ':'); i >= 0 {
			authority = authority[:i]
		}
		rest = authority + "/" + p
	} else if host, p, ok := strings.Cut(url, ":"); ok && !strings.ContainsAny(host, `/\`) && len(host) > 1 {
		// scp-like [user@]host:path; a one-letter host is a drive (C:\src).
		if i := strings.LastIndexByte(host, '@'); i >= 0 {
			host = host[i+1:]
		}
		rest = host + "/" + strings.TrimLeft(p, "/")
	} else {
		return egress.RepoPattern{}, false
	}
	// A remote names one repository: a * would let a remote allow a whole
	// owner or group.
	if strings.Contains(rest, "*") {
		return egress.RepoPattern{}, false
	}
	// A host without a dot is no forge's: an ssh_config alias, such as the
	// glov-<vm> remote itself.
	if host, _, _ := strings.Cut(rest, "/"); !strings.Contains(host, ".") {
		return egress.RepoPattern{}, false
	}
	return egress.ParseRepoPattern(rest)
}

// Host is the SSH host git reaches VM vm's guest repository through, in
// SSHConfigFile.
func Host(vm string) string { return "glov-" + vm }

// Remote is the name of the host repository's remote for VM vm.
func Remote(vm string) string { return "glov-" + vm }

// GuestBusyScript exits 0 and prints the name of a program of the guest
// user's that works in $1 or below (its current directory there), such as
// a harness; it exits 1 when there is none. A shell idle at its prompt
// there (no child, as one left in a herdr tab or a glov ssh session) does
// not count: what it runs next is in the project too and counts then.
// Linux (/proc). The macOS edition runs the same script.
const GuestBusyScript = `for p in /proc/[0-9]*; do
	pid=${p#/proc/}
	[ "$pid" = "$$" ] && continue
	c=$(readlink "$p/cwd" 2>/dev/null) || continue
	case "$c" in "$1"|"$1"/*) ;; *) continue ;; esac
	n=$(cat "$p/comm" 2>/dev/null) || continue
	case "$n" in sh|bash|dash|zsh|fish|ksh|mksh)
		grep -qs "^PPid:[[:space:]]*$pid$" /proc/[0-9]*/status || continue ;;
	esac
	echo "$n"
	exit 0
done
exit 1
`

// SSHHost is one Host block of glov's ssh_config.
type SSHHost struct {
	VM         string
	Key        string
	KnownHosts string
	// Relay is the ProxyCommand (glov ssh-relay <vm>).
	Relay string
}

// sshBegin and sshEnd bracket a VM's block in glov's ssh_config.
func sshBegin(vm string) string { return "# glov " + vm + ": begin (written by glov)" }
func sshEnd(vm string) string   { return "# glov " + vm + ": end" }

// WithSSHHost is glov's ssh_config text with h's block in place of the VM's
// old one, or added. The forwarding, escape and master-connection options
// keep the guest from reaching the host, as glov ssh does for an isolated
// VM; herdr-core's ssh to the VM's machine (glov claude --herdr) reads
// them too. ssh keeps an option's first value, and the Include goes first
// in ~/.ssh/config, so a Host * there cannot loosen them.
func WithSSHHost(text string, h SSHHost) string {
	text = WithoutSSHHost(text, h.VM)
	q := func(p string) string { return `"` + slashed(p) + `"` }
	block := []string{
		sshBegin(h.VM),
		"Host " + Host(h.VM),
		"  HostName " + h.VM,
		"  User glov",
		"  IdentityFile " + q(h.Key),
		"  IdentitiesOnly yes",
		"  UserKnownHostsFile " + q(h.KnownHosts),
		"  StrictHostKeyChecking accept-new",
		"  HostKeyAlias " + h.VM,
		"  ProxyCommand " + h.Relay,
		"  ClearAllForwardings yes",
		"  ForwardAgent no",
		"  ForwardX11 no",
		"  Tunnel no",
		"  PermitLocalCommand no",
		"  EnableEscapeCommandline no",
		"  ControlMaster no",
		"  ControlPath none",
		sshEnd(h.VM),
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + strings.Join(block, "\n") + "\n"
}

// WithoutSSHHost is glov's ssh_config text without VM vm's block.
func WithoutSSHHost(text, vm string) string {
	begin, end := sshBegin(vm), sshEnd(vm)
	var out []string
	skip := false
	for _, line := range strings.SplitAfter(text, "\n") {
		switch strings.TrimRight(line, "\r\n") {
		case begin:
			skip = true
			continue
		case end:
			if skip {
				skip = false
				continue
			}
		}
		if !skip && line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "")
}

// includePath is how ~/.ssh/config names glov's ssh_config at path: from
// ~ when path is in home. Git for Windows' ssh takes C:/... in an Include
// for a path relative to ~/.ssh; ~/... means the same file to it and to
// Windows' OpenSSH.
func includePath(path, home string) string {
	p, h := slashed(path), strings.TrimRight(slashed(home), "/")
	if h != "" && len(p) > len(h)+1 && strings.EqualFold(p[:len(h)], h) && p[len(h)] == '/' {
		return "~" + p[len(h):]
	}
	return p
}

// IncludeLine is the line ~/.ssh/config (in home) needs for ssh to read
// glov's ssh_config at path.
func IncludeLine(path, home string) string { return `Include "` + includePath(path, home) + `"` }

// HasInclude reports whether ~/.ssh/config text (in home) includes path,
// as IncludeLine names it.
func HasInclude(text, path, home string) bool {
	want := strings.ToLower(includePath(path, home))
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && strings.EqualFold(f[0], "include") {
			for _, p := range f[1:] {
				if strings.ToLower(strings.Trim(slashed(p), `"`)) == want {
					return true
				}
			}
		}
	}
	return false
}

// WithInclude is ~/.ssh/config text (in home) with IncludeLine first: an
// Include after a Host line would apply only within that Host.
func WithInclude(text, path, home string) string {
	return "# Added by glov: the SSH hosts of glov claude's VMs (glov-<vm>)\n" + IncludeLine(path, home) + "\n\n" + text
}

// winBase is a Windows path's last element, on any OS (the portable tests
// run elsewhere too).
func winBase(p string) string {
	p = strings.TrimRight(p, `\/`)
	return p[strings.LastIndexAny(p, `\/`)+1:]
}

// slashed is a Windows path with forward slashes, which ssh_config takes.
func slashed(p string) string { return strings.ReplaceAll(p, `\`, "/") }
