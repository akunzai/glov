package harness

import (
	"fmt"
	"path"
)

// The conversation records of an isolated VM's harnesses are kept on the
// host for review: a live share at SessionsGuestPath, with each harness's
// record directory (Profile.Sessions) a link into it. Its host folder is
// %LOCALAPPDATA%\glov\sessions\<vm>, which outlives glov vm delete, never
// below the host's own harness folders, so nothing the guest writes is
// where a harness on the host would read it back.
const (
	SessionsShare     = "harness-sessions"
	SessionsGuestPath = "/mnt/glov-sessions"
	// SessionsFolder holds a folder per VM in %LOCALAPPDATA%\glov, which is
	// glovd's data directory too: glovd allows this one share there.
	SessionsFolder = "sessions"
)

// SessionsTarget is where dir (below the guest home) of harness goes in
// the share.
func SessionsTarget(harness, dir string) string {
	return SessionsGuestPath + "/" + harness + "/" + path.Base(dir)
}

// SessionsRootScript runs as root: the share's guest folder, owned by user,
// so the user's links can be made into it before the first sync.
func SessionsRootScript(user string) string {
	return fmt.Sprintf("mkdir -p %s && chown %s:%s %s", SessionsGuestPath, user, user, SessionsGuestPath)
}

// SessionsLinkScript runs as the guest user: moves what dir already holds
// into the share, then makes dir a link to it.
func SessionsLinkScript(harness, dir string) string {
	return fmt.Sprintf(`set -e
t='%s'; d="$HOME/%s"
mkdir -p "$t" "$(dirname "$d")"
if [ -d "$d" ] && [ ! -L "$d" ]; then cp -a "$d"/. "$t"/; rm -rf "$d"; fi
ln -sfn "$t" "$d"
`, SessionsTarget(harness, dir), dir)
}

// SessionsUnlinkScript runs as the guest user on a VM without the share: a
// record directory still linked to it becomes a plain one again.
func SessionsUnlinkScript(dir string) string {
	return fmt.Sprintf(`d="$HOME/%s"
case "$(readlink "$d" 2>/dev/null)" in %s/*) rm -f "$d"; mkdir -p "$d" ;; esac
`, dir, SessionsGuestPath)
}
