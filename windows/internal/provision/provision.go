// Package provision prepares a new VM's first boot. glovd attaches a
// NoCloud seed disk holding a user-data script and the Guest Service; the
// image's cloud-init runs the script
// as root, which installs the Guest Service, sets up the boot, reports each
// step on the serial console (COM1) and powers the VM off. glovd reads
// the report to know how provisioning went.
package provision

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/seed"
)

// Report lines on COM1 start with Marker; Done and Failed end provisioning.
const (
	Marker = "GLOV-PROVISION:"
	Done   = "GLOV-PROVISION-OK"
	Failed = "GLOV-PROVISION-FAILED:"
)

// GuestFile is the Guest Service's name on the seed disk.
const GuestFile = "glov-guest"

// LogFile is the provisioning script's own output in the guest.
const LogFile = "/var/log/glov-provision.log"

// report starts both scripts: their output goes to LogFile, say reports a
// step on the console and fail ends provisioning, copying the log's tail to
// the console so glovd logs the cause. The serial getty can hang ttyS0
// up at any moment, which revokes descriptors already open on it, so every
// console line opens it afresh.
const report = `exec >>` + LogFile + ` 2>&1
console() { echo "$*" > /dev/ttyS0; }
say() { console "` + Marker + ` $*"; }
fail() { tail -n 20 ` + LogFile + ` > /dev/ttyS0; console "` + Failed + ` $*"; sync; poweroff; exit 1; }
`

// systemdUserData runs once, as root, in cloud-init's final stage on Debian
// and Ubuntu (systemd). POSIX sh.
//
//   - cloud-init waits for network-online.target before this stage, and
//     Ubuntu's cloud-init waits for the network itself before a script;
//     with no network adapter on this boot, networkConfig's dummy device
//     is what comes online.
//   - Later boots have no network adapter either (the guest gets out only
//     through the egress proxy over hv_sock): the script removes
//     cloud-init's network configuration, masks the wait-online service,
//     which would wait 2 minutes for a network, and turns cloud-init off,
//     which would bring both back.
//   - cloud-init grows the root partition on this boot only, once cloud-init
//     is off; glov vm configure --disk-size grows the disk later, so a
//     systemd service grows the partition and file system on every boot
//     that finds room after it.
//   - The kernel, its hv_sock module and the boot loader are the image's:
//     GRUB's menu timeout is already 0, and its console the serial port.
//   - sudo resolves the host name: the script maps it to 127.0.1.1, as
//     Debian's installer does.
const systemdUserData = report + systemdSteps

const systemdSteps = `
say "installing the Guest Service"
seed=$(mktemp -d) || fail "mktemp"
mount -o ro LABEL=` + seed.Label + ` "$seed" || fail "mount the seed disk"
install -m 0755 "$seed/` + GuestFile + `" /tmp/` + GuestFile + ` || fail "copy ` + GuestFile + `"
umount "$seed"
echo hv_sock > /etc/modules-load.d/glov.conf || fail "add hv_sock to /etc/modules-load.d"
modprobe hv_sock || fail "load hv_sock"
/tmp/` + GuestFile + ` install || fail "install the Guest Service"
rm -f /tmp/` + GuestFile + `
systemctl is-enabled --quiet ` + GuestFile + ` || fail "` + GuestFile + ` is not enabled"
say "Guest Service $(/usr/local/bin/` + GuestFile + ` --version)"

say "setting the hostname to $name"
echo "$name" > /etc/hostname || fail "write /etc/hostname"
hostname "$name" || fail "set the hostname"
sed -i '/^127\.0\.1\.1[[:space:]]/d' /etc/hosts || fail "edit /etc/hosts"
echo "127.0.1.1 $name" >> /etc/hosts || fail "write /etc/hosts"

say "installing the root file system resizer"
cat > /usr/local/sbin/glov-expand-root <<'GROW' || fail "write the resizer"
` + GrowRoot + `GROW
chmod 0755 /usr/local/sbin/glov-expand-root || fail "make the resizer executable"
cat > /etc/systemd/system/glov-expand-root.service <<'UNIT' || fail "write the resizer's unit"
[Unit]
Description=Grow the root file system to fill its disk (glov)
After=local-fs.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/glov-expand-root

[Install]
WantedBy=multi-user.target
UNIT
systemctl enable --quiet glov-expand-root.service || fail "enable the resizer"

say "turning off the network wait"
rm -f /etc/netplan/50-cloud-init.yaml || fail "remove cloud-init's netplan file"
systemctl mask --quiet systemd-networkd-wait-online.service || fail "mask systemd-networkd-wait-online"
touch /etc/cloud/cloud-init.disabled || fail "turn cloud-init off"

sync
console "` + Done + `"
poweroff
`

// ProxyURL is the in-guest egress proxy that isolated VMs' programs use.
const ProxyURL = "http://127.0.0.1:3128"

// egressSetup runs first in an isolated VM's script, before the Guest
// Service is installed, so the Guest Service finds the marker when it first
// starts (docs/guest-protocol.md "Isolated VM egress proxy"): the marker
// makes it serve the proxy on 127.0.0.1:3128; /etc/environment gives PAM
// logins the proxy, and the apt file gives apt.
const egressSetup = `
say "setting up the egress proxy"
install -d -m 0755 /etc/glov || fail "create /etc/glov"
printf 'glov-egress-v1\n' > /etc/glov/egress-proxy || fail "write the egress marker"
chmod 0644 /etc/glov/egress-proxy
cat >> /etc/environment <<'ENV' || fail "write /etc/environment"
HTTP_PROXY=` + ProxyURL + `
HTTPS_PROXY=` + ProxyURL + `
http_proxy=` + ProxyURL + `
https_proxy=` + ProxyURL + `
NO_PROXY=localhost,127.0.0.1,::1
no_proxy=localhost,127.0.0.1,::1
ENV
cat > /etc/apt/apt.conf.d/90glov-proxy <<'APT' || fail "write the apt proxy"
Acquire::http::Proxy "` + ProxyURL + `";
Acquire::https::Proxy "` + ProxyURL + `";
APT
`

// CAPath is where an isolated VM's first boot writes the egress CA for
// update-ca-certificates; the Guest Service's egress.Env adds TrustEnv
// when it is there, as macOS's CloudInitSeed does.
const CAPath = "/usr/local/share/ca-certificates/glov-egress-ca.crt"

// trustSetup writes the egress CA, which the proxy signs the certificates
// of hosts a granted secret is bound to with, into the system trust store,
// and points programs that read a CA file of their own at the system
// bundle: Node adds it to its bundled roots, OpenSSL and Python requests
// read it.
func trustSetup(ca []byte) string {
	if len(ca) == 0 {
		return ""
	}
	const bundle = "/etc/ssl/certs/ca-certificates.crt"
	return `
say "trusting the egress CA"
install -d -m 0755 ` + path.Dir(CAPath) + ` || fail "create the CA directory"
printf '%s' '` + base64.StdEncoding.EncodeToString(ca) + `' | base64 -d > ` + CAPath + ` || fail "write the egress CA"
chmod 0644 ` + CAPath + `
update-ca-certificates >/dev/null || fail "update the CA certificates"
cat >> /etc/environment <<'ENV' || fail "write the trust environment"
NODE_EXTRA_CA_CERTS=` + bundle + `
REQUESTS_CA_BUNDLE=` + bundle + `
SSL_CERT_FILE=` + bundle + `
ENV
`
}

// networkConfig is NoCloud's network configuration for the provisioning
// boot, which has no network adapter: one dummy device, routable and with a
// name server, so systemd-networkd-wait-online (and on Ubuntu its netplan
// drop-in, which asks for both) finds the network online at once instead of
// after 2 minutes. 192.0.2.1 is TEST-NET-1; nothing is ever sent there.
const networkConfig = `network:
  version: 2
  dummy-devices:
    glovdummy0:
      addresses: [192.0.2.1/32]
      nameservers:
        addresses: [192.0.2.1]
`

// GrowRoot grows the root partition and its ext4 file system to fill the
// disk with cloud-init's growpart, on Debian and Ubuntu; growpart reports
// NOCHANGE when there is no room. It runs from a systemd service.
const GrowRoot = `#!/bin/sh
# Installed by glov: grows / after glov vm configure --disk-size.
dev=$(findmnt -no SOURCE /)
blk=/sys/class/block/${dev#/dev/}
[ -r "$blk/partition" ] || exit 0
part=$(cat "$blk/partition")
disk=/dev/$(basename "$(readlink -f "$blk/..")")
if out=$(growpart "$disk" "$part" 2>&1); then
	echo "glov: grew $dev"
else
	case $out in NOCHANGE*) ;; *) echo "$out" >&2; exit 1 ;; esac
fi
# Also after NOCHANGE: a boot stopped between growpart and resize2fs
# leaves the partition grown and the file system not.
resize2fs "$dev"
`

// VM is a new VM to provision.
type VM struct {
	// ID is the VM's GUID, its HCS compute system ID.
	ID   string
	Name string
	// Distro is one of api.Distros.
	Distro string
	// Dir is the VM's directory; the seed disk is written there and
	// removed afterwards.
	Dir       string
	Disk      string
	BaseDisk  string
	CPUs      int
	MemoryMiB int
	// Isolated sets up the egress proxy (Debian and Ubuntu).
	Isolated bool
	// Harnesses (isolated only) are set up for User.
	Harnesses []harness.Profile
	// EgressCA (isolated only) is the egress CA's certificate, PEM.
	EgressCA []byte
}

// Hostname is the guest hostname for the VM called name: one DNS label, so
// '.' and '_' become '-'. It holds only a-z 0-9 and '-'.
func Hostname(name string) string {
	h := strings.Map(func(r rune) rune {
		if r == '.' || r == '_' {
			return '-'
		}
		return r
	}, name)
	if len(h) > 63 {
		h = h[:63]
	}
	return strings.TrimRight(h, "-")
}

// User is the guest account harnesses run as: the one glov ssh signs in
// as, which a harness VM creates at provisioning and signs in on tty1.
const User = "glov"

// AutologinPath is the getty drop-in that signs User in on tty1, so glov
// exec --as user has a session to run in; ttyS0 stays a plain console.
const AutologinPath = "/etc/systemd/system/getty@tty1.service.d/glov-autologin.conf"

// UserNamespacesPath lets unprivileged users create user namespaces, which
// Ubuntu restricts through AppArmor, for a harness whose sandbox needs them
// (Profile.UserNamespaces); it eases privilege escalation in the guest, so
// only such VMs get it.
const UserNamespacesPath = "/etc/sysctl.d/60-glov-userns.conf"

// harnessSetup is an isolated VM's harness step, as macOS's cloud-init
// does it: the user (no password, sudo without one), the profiles'
// environment in /etc/environment, their files in the user's home (each
// written from base64, so no content can end a heredoc), the tty1
// sign-in and, if a harness needs them, user namespaces. Nothing without
// harnesses.
func harnessSetup(harnesses []harness.Profile) string {
	if len(harnesses) == 0 {
		return ""
	}
	var names []string
	env := map[string]string{}
	userns := false
	for _, h := range harnesses {
		names = append(names, h.Name)
		maps.Copy(env, h.GuestEnvironment())
		userns = userns || h.UserNamespaces
	}
	var b strings.Builder
	fmt.Fprintf(&b, `
say "setting up the harnesses: %s"
u=%s
home=/home/$u
id "$u" >/dev/null 2>&1 || useradd -m -s /bin/bash -G sudo "$u" || fail "create the user $u"
sed -i "s/^$u:![^:]*:/$u:*:/" /etc/shadow || fail "unlock $u for key and console sign-in"
echo "$u ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/glov || fail "write the sudoers file"
chmod 0440 /etc/sudoers.d/glov
cat >> /etc/environment <<'ENV' || fail "write the harness environment"
`, strings.Join(names, ", "), User)
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	b.WriteString("ENV\n")
	var tops []string
	for _, h := range harnesses {
		for _, f := range h.GuestFiles() {
			dir := path.Dir(f.Path)
			if dir != "." {
				fmt.Fprintf(&b, "mkdir -p \"$home/%s\" || fail \"create %s\"\n", dir, dir)
			}
			fmt.Fprintf(&b, "printf '%%s' '%s' | base64 -d > \"$home/%s\" || fail \"write %s\"\n",
				base64.StdEncoding.EncodeToString([]byte(f.Content())), f.Path, f.Path)
			fmt.Fprintf(&b, "chmod 0600 \"$home/%s\"\n", f.Path)
			if top, _, _ := strings.Cut(f.Path, "/"); !slices.Contains(tops, top) {
				tops = append(tops, top)
			}
		}
	}
	for _, h := range harnesses {
		for _, l := range h.SecretLinks() {
			// The file appears once the Guest Service delivers the secret.
			if dir := path.Dir(l.Path); dir != "." {
				fmt.Fprintf(&b, "mkdir -p \"$home/%s\" || fail \"create %s\"\n", dir, dir)
			}
			fmt.Fprintf(&b, "ln -sfn %s \"$home/%s\" || fail \"link %s\"\n", l.Target, l.Path, l.Path)
			if top, _, _ := strings.Cut(l.Path, "/"); !slices.Contains(tops, top) {
				tops = append(tops, top)
			}
		}
	}
	for _, top := range tops {
		fmt.Fprintf(&b, "chown -hR \"$u:$u\" \"$home/%s\" || fail \"hand %s to $u\"\n", top, top)
	}
	fmt.Fprintf(&b, `mkdir -p %s || fail "create the getty drop-in directory"
cat > %s <<'GETTY' || fail "write the tty1 sign-in"
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin %s --noclear %%I $TERM
GETTY
`, path.Dir(AutologinPath), AutologinPath, User)
	if userns {
		fmt.Fprintf(&b, "echo 'kernel.apparmor_restrict_unprivileged_userns = 0' > %s || fail \"allow user namespaces\"\n", UserNamespacesPath)
	}
	return b.String()
}

// Files returns the seed disk's files for the VM called name: its ID
// and hostname as NoCloud meta-data, the script, and the Guest Service
// binary, and the network configuration. An isolated VM also gets the egress proxy set up and, with
// ca, trusts the egress CA.
func Files(id, name string, isolated bool, harnesses []harness.Profile, ca, guest []byte) []seed.File {
	hostname := Hostname(name)
	meta := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", id, hostname)
	script := systemdUserData
	if isolated {
		script = report + egressSetup + trustSetup(ca) + harnessSetup(harnesses) + systemdSteps
	}
	return []seed.File{
		{Name: "meta-data", Data: []byte(meta)},
		// hostname is safe inside single quotes.
		{Name: "user-data", Data: []byte("#!/bin/sh\nname='" + hostname + "'\n" + script)},
		{Name: GuestFile, Data: guest},
		{Name: "network-config", Data: []byte(networkConfig)},
	}
}

// ErrNoReport means the console ended without Done or Failed.
var ErrNoReport = errors.New("the VM stopped without reporting how provisioning went")

// Watch reads the serial console until the script reports Done or Failed.
// progress receives each step; every console line goes to log. It returns
// nil on Done.
func Watch(console io.Reader, progress func(string), log func(string)) error {
	sc := bufio.NewScanner(console)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		log(line)
		switch {
		case strings.HasPrefix(line, Done):
			return nil
		case strings.HasPrefix(line, Failed):
			return fmt.Errorf("provisioning failed: %s", strings.TrimSpace(strings.TrimPrefix(line, Failed)))
		case strings.HasPrefix(line, Marker):
			progress(strings.TrimSpace(strings.TrimPrefix(line, Marker)))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return ErrNoReport
}
