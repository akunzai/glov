// Package share holds a VM's shares on a Windows host: host folders that
// glov share sync mirrors into the guest, each at /mnt/<drive>/<path> (the
// WSL convention). A share is also
// part of the VM's copy allowlist. Paths here are canonical Windows paths
// (X:\dir\...), as Canonical returns them, compared without case as NTFS
// compares names.
package share

import (
	"fmt"
	"path"
	"strings"
)

// Mode says whether copies from the guest may write into a share's folder.
type Mode string

const (
	// RO: glov cp from the guest never writes into the folder.
	RO Mode = "ro"
	// RW: glov cp from the guest may write into the folder too.
	RW Mode = "rw"
	// Live: kept in step both ways while the VM runs (glov share agent),
	// and writable by glov cp as RW is.
	Live Mode = "live"
)

// Share is one entry of a VM record's shares.
type Share struct {
	Name     string `json:"name"`
	HostPath string `json:"hostPath"`
	Mode     Mode   `json:"mode"`
	// Exclude holds gitignore patterns, relative to the share root
	// (CompileExcludes).
	Exclude []string `json:"exclude,omitempty"`
	// Guest is where glov puts the share in the guest instead of
	// GuestPath's /mnt/<drive>/<path>, such as a harness VM's conversation
	// records at /mnt/glov-sessions; glov share add sets none.
	Guest string `json:"guest,omitempty"`
	// GitDirs are git directories in a live share, relative to its root
	// with "/" ("" for the root itself): the share agent keeps the two
	// sides' git from changing one at once (live.Engine.GitDirs).
	GitDirs []string `json:"gitDirs,omitempty"`
}

// GuestPath is where s appears in the guest.
func (s Share) GuestPath() string {
	if s.Guest != "" {
		return s.Guest
	}
	p, _ := GuestPath(s.HostPath)
	return p
}

// checkGuest refuses a guest path that is not an absolute, clean path
// below /mnt.
func checkGuest(p string) error {
	if !strings.HasPrefix(p, "/mnt/") || path.Clean(p) != p || strings.ContainsAny(p, "\\") {
		return fmt.Errorf("guest path %q: give a clean absolute path below /mnt", p)
	}
	return nil
}

// MaxNameLen bounds a share name.
const MaxNameLen = 64

// ValidName checks a share name: 1 to 64 letters, digits, '.', '_' or '-',
// not starting with '.' and not a Windows device name. The name is a file
// name on both sides (the hash caches and the guest manifest), and share
// sync reads <name>/<subdir>.
func ValidName(name string) error {
	ok := name != "" && len(name) <= MaxNameLen && name[0] != '.'
	for i := 0; ok && i < len(name); i++ {
		c := name[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
	}
	base, _, _ := strings.Cut(strings.ToLower(name), ".")
	if !ok || reservedName(base) {
		return fmt.Errorf("invalid share name %q: use 1 to %d letters, digits, '.', '_' or '-', not starting with '.'", name, MaxNameLen)
	}
	return nil
}

func reservedName(base string) bool {
	switch base {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '0' && base[3] <= '9'
}

// components splits a canonical path X:\a\b into "X:" and its directories,
// or reports that it is not one.
func components(path string) (string, []string, error) {
	bad := fmt.Errorf("host path %q is not an absolute path on a drive, such as C:\\src\\proj", path)
	if len(path) < 2 || path[1] != ':' || !(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z') {
		return "", nil, bad
	}
	rest := path[2:]
	if rest != "" && rest[0] != '\\' {
		return "", nil, bad
	}
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, `\`), `\`)
	if rest == "" {
		return path[:2], nil, nil
	}
	dirs := strings.Split(rest, `\`)
	for _, d := range dirs {
		if d == "" || d == "." || d == ".." || strings.ContainsAny(d, `/:`) {
			return "", nil, bad
		}
	}
	return path[:2], dirs, nil
}

// GuestPath maps a host folder to its guest path: C:\src\Proj is
// /mnt/c/src/Proj. A drive root is refused: a sync deletes what the host
// folder lacks, and the share is part of the copy allowlist.
func GuestPath(host string) (string, error) {
	drive, dirs, err := components(host)
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("host path %s is a drive root; share a folder of its own", host)
	}
	return "/mnt/" + strings.ToLower(drive[:1]) + "/" + strings.Join(dirs, "/"), nil
}

// Within reports whether path is dir or lies inside it, without case.
func Within(path, dir string) bool {
	p, d := strings.ToLower(strings.TrimSuffix(path, `\`)), strings.ToLower(strings.TrimSuffix(dir, `\`))
	return p == d || strings.HasPrefix(p, d+`\`)
}

// Protected is a host folder a share may not be (Tree false), or may be
// neither, nor lie inside, nor hold (Tree true).
type Protected struct {
	Path string
	Tree bool
	// Why ends the refusal: "is <Why>".
	Why string
}

// Check validates s as a new share of a VM that has existing, refusing a
// folder in protected, and returns it with its mode set. s.HostPath must be
// canonical. Host folders may not overlap: copies may write only into rw
// folders, and a rw share around a ro one would open it to writes.
func Check(s Share, existing []Share, protected []Protected) (Share, error) {
	if err := ValidName(s.Name); err != nil {
		return Share{}, err
	}
	switch s.Mode {
	case "":
		s.Mode = RW
	case RO, RW, Live:
	default:
		return Share{}, fmt.Errorf("share mode %q: want ro, rw or live", s.Mode)
	}
	if _, err := GuestPath(s.HostPath); err != nil {
		return Share{}, err
	}
	if s.Guest != "" {
		if err := checkGuest(s.Guest); err != nil {
			return Share{}, err
		}
	}
	for _, p := range protected {
		if p.Path == "" {
			continue
		}
		inside, holds := Within(s.HostPath, p.Path), Within(p.Path, s.HostPath)
		if inside && holds {
			return Share{}, fmt.Errorf("host path %s is %s; share a folder of its own", s.HostPath, p.Why)
		}
		if p.Tree && (inside || holds) {
			return Share{}, fmt.Errorf("host path %s holds or lies inside %s, which is %s; share a folder of its own", s.HostPath, p.Path, p.Why)
		}
	}
	if _, err := CompileExcludes(s.Exclude); err != nil {
		return Share{}, err
	}
	for _, g := range s.GitDirs {
		if s.Mode != Live {
			return Share{}, fmt.Errorf("share %s: git directories are for a live share", s.Name)
		}
		if g != "" && (path.Clean(g) != g || path.IsAbs(g) || g == ".." || strings.HasPrefix(g, "../") || strings.Contains(g, "\\")) {
			return Share{}, fmt.Errorf("share %s: git directory %q is not a clean path inside it", s.Name, g)
		}
	}
	for _, o := range existing {
		if o.Name == s.Name {
			return Share{}, fmt.Errorf("the VM already has a share named %s", s.Name)
		}
		if Within(s.HostPath, o.HostPath) || Within(o.HostPath, s.HostPath) {
			return Share{}, fmt.Errorf("host folder %s overlaps share %s at %s: share a folder once", s.HostPath, o.Name, o.HostPath)
		}
		if g, og := s.GuestPath(), o.GuestPath(); g == og || strings.HasPrefix(g, og+"/") || strings.HasPrefix(og, g+"/") {
			return Share{}, fmt.Errorf("guest path %s overlaps share %s at %s", g, o.Name, og)
		}
	}
	return s, nil
}

// Find returns the share called name, or an error listing the VM's shares.
func Find(shares []Share, name string) (Share, error) {
	var names []string
	for _, s := range shares {
		if s.Name == name {
			return s, nil
		}
		names = append(names, s.Name)
	}
	if len(names) == 0 {
		return Share{}, fmt.Errorf("no share named %q (this VM has no shares)", name)
	}
	return Share{}, fmt.Errorf("no share named %q (shares: %s)", name, strings.Join(names, ", "))
}

// Allowed reports whether a copy may read (write false) or write the
// canonical host path: inside one of extra, or inside a share's folder, a
// rw one for writes.
func Allowed(path string, shares []Share, extra []string, write bool) bool {
	for _, d := range extra {
		if d != "" && Within(path, d) {
			return true
		}
	}
	for _, s := range shares {
		if (!write || s.Mode == RW || s.Mode == Live) && Within(path, s.HostPath) {
			return true
		}
	}
	return false
}

// Describe is the line glov share list prints for s.
func (s Share) Describe() string {
	line := fmt.Sprintf("%s (%s): %s -> %s", s.Name, s.Mode, s.HostPath, s.GuestPath())
	if len(s.Exclude) > 0 {
		line += ", exclude " + strings.Join(s.Exclude, " ")
	}
	return line
}
