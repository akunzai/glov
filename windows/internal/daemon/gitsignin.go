package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/akunzai/glov/windows/internal/gitsignin"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/store"
)

// gitSyncTimeout bounds one git sign-in sync.
const gitSyncTimeout = 30 * time.Second

// putScript writes each "path mode base64" triple of its arguments where
// the file differs, relative to the directory it runs in; a base64 of "-"
// removes a file glov wrote (one holding a placeholder), and empty content
// creates no file. It prints "changed" for each file it changed.
const putScript = `set -e
put() {
  if [ "$3" = - ]; then
    if [ -f "$1" ] && grep -q 'glov-secret:' "$1"; then rm -f "$1"; echo changed; fi
    return 0
  fi
  if [ -z "$3" ] && [ ! -e "$1" ]; then return 0; fi
  mkdir -p "$(dirname "$1")"
  t="$1.glov~"
  printf '%s' "$3" | base64 -d > "$t"
  if cmp -s "$t" "$1"; then rm -f "$t"; return 0; fi
  chmod "$2" "$t"
  mv -f "$t" "$1"
  echo changed
}
while [ $# -ge 3 ]; do put "$1" "$2" "$3"; shift 3; done
`

// userPutScript is putScript in the guest user's home.
var userPutScript = "cd /home/" + provision.User + "\n" + putScript

// gitSignIns are the git secrets granted to r: false when r has no egress
// settings or the definitions cannot be read.
func (d *Daemon) gitSignIns(r store.Record) ([]gitsignin.Entry, bool) {
	if r.Egress == nil || d.Secrets == nil {
		return nil, false
	}
	all, err := d.Secrets.List()
	if err != nil {
		return nil, false
	}
	var out []gitsignin.Entry
	for _, def := range all {
		if def.Forge != "" && slices.Contains(r.Egress.Secrets, def.Name) {
			out = append(out, gitsignin.Entry{Name: def.Name, Forge: def.Forge, Hosts: def.Hosts})
		}
	}
	return out, true
}

// syncGitSignIns sets git, gh and glab in the running VM's guest up for its
// granted git secrets (gitsignin), writing only files that differ; one sync
// per VM at a time, and a stopped VM is left for its next Guest Service
// session. The guest user's files are written as that user.
func (d *Daemon) syncGitSignIns(owner, name string) error {
	m, _ := d.gitSyncs.LoadOrStore(name, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	defer m.(*sync.Mutex).Unlock()
	r, err := d.Store.Load(owner, name)
	if err != nil || !r.Isolated || r.ID == "" || !d.VMs.Running(r.ID) {
		return nil
	}
	entries, ok := d.gitSignIns(r)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitSyncTimeout)
	defer cancel()
	changed, err := d.applyGitSignIns(ctx, r, entries)
	if err != nil {
		slog.Warn("cannot set up git sign-ins", "vm", name, "err", err)
		return fmt.Errorf("cannot set up git sign-ins: %v", err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name)
	}
	slices.Sort(names)
	slog.Info("git sign-ins synced", "vm", name, "secrets", names, "changed", changed)
	return nil
}

// readGuestFile is the guest's file at path, read as root; "" when there
// is none.
func (d *Daemon) readGuestFile(ctx context.Context, r store.Record, path string) (string, error) {
	var lines []string
	code, err := d.runGuest(ctx, r, []string{"sh", "-c", `[ ! -e "$1" ] || base64 "$1"`, "sh", path},
		func(line string) { lines = append(lines, line) })
	if err == nil && code != 0 {
		err = fmt.Errorf("reading %s exited %d", path, code)
	}
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(lines, ""))
	if err != nil {
		return "", fmt.Errorf("reading %s: %v", path, err)
	}
	return string(data), nil
}

// applyGitSignIns reads the guest's /etc/gitconfig, then writes what
// differs: the system files as root, the guest user's as that user. It
// reports whether any file changed.
func (d *Daemon) applyGitSignIns(ctx context.Context, r store.Record, entries []gitsignin.Entry) (bool, error) {
	run := func(what string, argv []string) (bool, error) {
		var out []string
		code, err := d.runGuest(ctx, r, argv, func(line string) { out = append(out, line) })
		if err == nil && code != 0 {
			err = fmt.Errorf("%s exited %d: %s", what, code, strings.Join(out[max(0, len(out)-3):], "; "))
		}
		return slices.Contains(out, "changed"), err
	}
	current, err := d.readGuestFile(ctx, r, gitsignin.GitConfigPath)
	if err != nil {
		return false, err
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	system := []string{"sh", "-c", putScript, "sh"}
	if len(entries) > 0 {
		system = append(system, gitsignin.HelperPath, "755", b64(gitsignin.Helper))
	}
	system = append(system, gitsignin.SignInsPath, "644", b64(gitsignin.SignIns(entries)))
	if wanted := gitsignin.GitConfig(current, entries); wanted != current {
		system = append(system, gitsignin.GitConfigPath, "644", b64(wanted))
	}
	changed, err := run("writing the system files", system)
	if err != nil {
		return false, err
	}
	// As the guest user, so its home stays its own. Only a VM made with
	// harnesses has that user; git's files above serve any account.
	user := []string{"sh", "-c", `id -u "$1" >/dev/null 2>&1 || exit 0; u=$1; shift; exec runuser -u "$u" -- sh -c "$@"`,
		"sh", provision.User, userPutScript, "sh"}
	for _, f := range []struct {
		path   string
		render func([]gitsignin.Entry) (string, bool)
	}{{gitsignin.GHHostsPath, gitsignin.GHHosts}, {gitsignin.GlabConfigPath, gitsignin.GlabConfig}} {
		content := "-"
		if text, ok := f.render(entries); ok {
			content = b64(text)
		}
		user = append(user, f.path, "600", content)
	}
	userChanged, err := run("writing the user's files", user)
	return changed || userChanged, err
}
