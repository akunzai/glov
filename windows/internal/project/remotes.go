package project

import (
	"encoding/json/v2"
	"errors"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
)

// Remotes are the repositories of project remotes the owner has let a
// project's VM reach (glov claude on a new VM), kept on the host: a file
// mapping a project's path to its confirmed repositories (RepoPattern
// text), so a remote added since shows as new.
type Remotes struct{ Path string }

// Confirmed are the repositories confirmed for project.
func (r Remotes) Confirmed(project string) ([]string, error) {
	all, err := r.load()
	return all[strings.ToLower(project)], err
}

// Confirm adds repos to project's confirmed ones.
func (r Remotes) Confirm(project string, repos []string) error {
	all, err := r.load()
	if err != nil {
		return err
	}
	key := strings.ToLower(project)
	set := map[string]bool{}
	for _, s := range append(all[key], repos...) {
		set[s] = true
	}
	all[key] = slices.Sorted(maps.Keys(set))
	b, err := json.Marshal(all, json.Deterministic(true))
	if err != nil {
		return err
	}
	tmp := r.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.Path)
}

func (r Remotes) load() (map[string][]string, error) {
	all := map[string][]string{}
	b, err := os.ReadFile(r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return all, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	return all, nil
}

// ApplyRemotes says whether to let the VM reach the remotes: at a terminal
// the owner's answer (Enter accepts unless one is new, not confirmed for
// the project before); without one, only with --allow-remotes.
func ApplyRemotes(anyNew, terminal, allowFlag bool, answer func() string) bool {
	if allowFlag {
		return true
	}
	if !terminal {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer())) {
	case "":
		return !anyNew
	case "y", "yes":
		return true
	}
	return false
}

// WithoutCredentials is remote URL u without a user or password written
// into an http(s) URL (https://user:token@host/..., or a token alone as
// the user), which must not reach the guest. An SSH URL's user (ssh://git@
// or scp-like git@host:path) is the account, not a secret, and is kept.
func WithoutCredentials(u string) string {
	parsed, err := url.Parse(u)
	if err != nil || parsed.User == nil || (!strings.EqualFold(parsed.Scheme, "https") && !strings.EqualFold(parsed.Scheme, "http")) {
		return u
	}
	parsed.User = nil
	return parsed.String()
}
