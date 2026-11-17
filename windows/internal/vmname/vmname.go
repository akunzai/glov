// Package vmname validates and resolves VM names. A name ends up in file
// paths, pipe names and HCS IDs, so it is checked before any of those.
package vmname

import (
	"fmt"
	"strings"
)

// MaxLen is the longest accepted name.
const MaxLen = 64

// reserved are Windows device names, which cannot be file or directory names
// with any extension.
var reserved = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func init() {
	for _, p := range []string{"com", "lpt"} {
		for _, d := range "0123456789¹²³" {
			reserved[p+string(d)] = true
		}
	}
}

// Valid reports whether name is a valid VM name: 1 to 64 characters from
// a-z 0-9 . _ -, starting with a letter or digit, not ending with '.', and not
// a Windows device name. Upper case is refused because Windows paths are case
// insensitive, so "Dev" and "dev" would share a directory.
func Valid(name string) error {
	ok := name != "" && len(name) <= MaxLen && alnum(name[0]) && name[len(name)-1] != '.'
	for i := 0; ok && i < len(name); i++ {
		c := name[i]
		ok = alnum(c) || c == '.' || c == '_' || c == '-'
	}
	base, _, _ := strings.Cut(name, ".")
	if !ok || reserved[base] {
		return fmt.Errorf("invalid VM name %q: use 1 to %d characters from a-z 0-9 . _ -, starting with a letter or digit, not ending with '.'", name, MaxLen)
	}
	return nil
}

func alnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

// Resolve maps a reference to one of names: an exact name wins, otherwise a
// prefix that matches exactly one name.
func Resolve(ref string, names []string) (string, error) {
	var matches []string
	for _, n := range names {
		if n == ref {
			return n, nil
		}
		if strings.HasPrefix(n, ref) {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(names) == 0 {
			return "", fmt.Errorf("no VM named %q; there are no VMs", ref)
		}
		return "", fmt.Errorf("no VM named %q; VMs: %s", ref, strings.Join(names, ", "))
	default:
		return "", fmt.Errorf("%q matches several VMs: %s", ref, strings.Join(matches, ", "))
	}
}
