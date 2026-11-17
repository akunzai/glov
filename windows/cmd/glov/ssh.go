package main

import (
	"path/filepath"
	"strings"
)

// sshUser is the guest account glov ssh signs in as, as on macOS.
const sshUser = "glov"

// sshFiles are where glov keeps a VM's SSH client files: in the user's own
// profile, not the VM's directory, because glovd's data directory
// belongs to SYSTEM and ssh runs as the user.
type sshFiles struct{ dir string }

func (f sshFiles) key() string        { return filepath.Join(f.dir, "id_ed25519") }
func (f sshFiles) knownHosts() string { return filepath.Join(f.dir, "known_hosts") }

// sshSetup makes $1 (a public key) able to sign in as sshUser over SSH, as
// root in the guest, and is safe to run again. POSIX sh (dash).
//
//   - useradd leaves the password "!", a locked account that sshd turns
//     away without PAM; "*" allows no password but leaves keys working.
const sshSetup = `set -e
u=` + sshUser + `
if ! id "$u" >/dev/null 2>&1; then
	useradd -m -s "$(command -v bash || echo /bin/sh)" "$u"
fi
sed -i "s/^$u:![^:]*:/$u:*:/" /etc/shadow
echo "$u ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/glov
chmod 0440 /etc/sudoers.d/glov
home=$(getent passwd "$u" | cut -d: -f6)
keys=$home/.ssh/authorized_keys
install -d -m 0700 -o "$u" -g "$(id -gn "$u")" "$home/.ssh"
grep -qxF "$1" "$keys" 2>/dev/null || printf '%s\n' "$1" >> "$keys"
chown "$u:$(id -gn "$u")" "$keys"
chmod 0600 "$keys"
command -v sshd >/dev/null || { echo "the guest has no sshd: install OpenSSH's server" >&2; exit 3; }
systemctl is-active --quiet ssh.socket || systemctl is-active --quiet ssh || systemctl start ssh
`

// sshArgs is the ssh argument vector (without argv[0]) for vm, reached
// through relay, a ProxyCommand. extra is what the user wrote after --: its
// leading ssh options go before ours end, the rest is the remote command.
// The user is passed with -l and the host comes last after --, so a host
// can never be read as an option. Host keys stay in f, under the VM's name.
// ssh keeps an option's first value, so ours win over extra and ssh_config.
// An isolated VM's session gets isolatedSSHOptions and, after extra's
// options, isolatedSSHFlags.
func sshArgs(vm string, f sshFiles, relay string, isolated bool, extra []string) []string {
	a := []string{
		"-i", f.key(),
		"-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + sshQuote(f.knownHosts()),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=" + vm,
		"-o", "ProxyCommand=" + relay,
		"-l", sshUser,
	}
	var last []string
	if isolated {
		for _, o := range isolatedSSHOptions {
			a = append(a, "-o", o)
		}
		last = isolatedSSHFlags
	}
	options, command := splitSSHOptions(extra)
	a = append(a, options...)
	a = append(a, last...)
	a = append(a, "--", vm)
	return append(a, command...)
}

// isolatedSSHOptions are forced on an isolated VM's session: no forwarding
// of ports, agent, X11 or tunnels, no local commands and no ~C command line,
// any of which would give the guest a way to the host.
var isolatedSSHOptions = []string{
	"ClearAllForwardings=yes", "ForwardAgent=no", "ForwardX11=no", "Tunnel=no", "PermitLocalCommand=no", "EnableEscapeCommandline=no",
}

// isolatedSSHFlags go after the user's options: -A, -X, -Y and -S set their
// option outright, whatever came first, so the last of each wins; -S none
// keeps a master connection opened without the options above out.
var isolatedSSHFlags = []string{"-a", "-x", "-S", "none"}

// sshRelayCommand is the ProxyCommand that runs glov (the absolute path of
// the running glov) ssh-relay vm.
func sshRelayCommand(glov, vm string) string {
	return sshQuote(glov) + " ssh-relay " + vm
}

// sshQuote quotes a path for an ssh option value: ssh splits values at
// spaces (C:\Program Files) and expands % tokens in them.
func sshQuote(path string) string {
	return `"` + strings.ReplaceAll(path, "%", "%%") + `"`
}

// sshValueFlags are ssh options that take a separate value when written as
// exactly -X.
const sshValueFlags = "BbcDEeFIiJLlmOopQRSWw"

// splitSSHOptions splits words into its leading ssh options (with their
// values) and the remote command after them.
func splitSSHOptions(words []string) (options, command []string) {
	i := 0
	for i < len(words) && strings.HasPrefix(words[i], "-") && words[i] != "-" {
		if words[i] == "--" {
			return words[:i], words[i+1:]
		}
		if w := words[i]; len(w) == 2 && strings.ContainsRune(sshValueFlags, rune(w[1])) {
			i += 2
		} else {
			i++
		}
	}
	i = min(i, len(words))
	return words[:i], words[i:]
}
