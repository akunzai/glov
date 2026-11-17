package share

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// HostFile is a file of a share's host folder as a sync sees it.
type HostFile struct {
	Size int64
	Hash string
}

// GuestFile is a file of the guest copy, from sync.manifest.
type GuestFile struct {
	Size int64
	// Hash is empty when the guest could not read the file: it is written
	// again.
	Hash string
	// Modified: changed in the guest since the last sync.
	Modified bool
}

// Plan is what a sync does to make the guest copy mirror the host folder:
// host files whose SHA-256 the guest lacks are written, guest files and
// directories the host lacks are removed, and every guest path that is
// excluded, or lies under a host path the sync skips (untouched), is left
// alone. Paths are relative to the share root, '/'-separated. It is the
// macOS edition's SyncPlan (Sources/GlovCore/SyncPlan.swift).
type Plan struct {
	// Deletes are guest files to delete, sorted.
	Deletes []string
	// Rmdirs are guest directories to remove when empty, deepest first.
	Rmdirs []string
	// Mkdirs are directories to create, parents first.
	Mkdirs []string
	// Writes are host files to send, sorted.
	Writes []string
	// Unchanged counts files already equal on both sides.
	Unchanged int
	// Overwritten are guest-modified files the plan replaces or deletes,
	// sorted.
	Overwritten []string
}

// NewPlan compares the two sides.
func NewPlan(hostFiles map[string]HostFile, hostDirs map[string]bool, guestFiles map[string]GuestFile, guestDirs map[string]bool,
	excludes Excludes, untouched []string) Plan {
	kept := func(path string, dir bool) bool {
		if excludes.ExcludedOrParent(path, dir) {
			return true
		}
		for _, u := range untouched {
			if path == u || strings.HasPrefix(path, u+"/") {
				return true
			}
		}
		return false
	}
	var p Plan
	for path, g := range guestFiles {
		if kept(path, false) {
			continue
		}
		if h, ok := hostFiles[path]; ok {
			if g.Hash != "" && g.Hash == h.Hash {
				p.Unchanged++
				continue
			}
		} else {
			p.Deletes = append(p.Deletes, path)
		}
		if g.Modified {
			p.Overwritten = append(p.Overwritten, path)
		}
	}
	for path, h := range hostFiles {
		if kept(path, false) {
			continue
		}
		if g, ok := guestFiles[path]; ok && g.Hash != "" && g.Hash == h.Hash {
			continue
		}
		p.Writes = append(p.Writes, path)
	}
	for d := range guestDirs {
		if !hostDirs[d] && !kept(d, true) {
			p.Rmdirs = append(p.Rmdirs, d)
		}
	}
	for d := range hostDirs {
		if !guestDirs[d] && !kept(d, true) {
			p.Mkdirs = append(p.Mkdirs, d)
		}
	}
	depth := func(s string) int { return strings.Count(s, "/") }
	slices.Sort(p.Deletes)
	slices.Sort(p.Writes)
	slices.Sort(p.Overwritten)
	slices.SortFunc(p.Rmdirs, func(a, b string) int { return cmp.Or(cmp.Compare(depth(b), depth(a)), cmp.Compare(b, a)) })
	slices.SortFunc(p.Mkdirs, func(a, b string) int { return cmp.Or(cmp.Compare(depth(a), depth(b)), cmp.Compare(a, b)) })
	return p
}

// ParseRef reads share sync's <name>[/<subdir>] (either slash) into the
// share name and the subdirectory, relative and '/'-separated, empty for the
// whole share.
func ParseRef(ref string) (name, subdir string, err error) {
	text := strings.TrimSuffix(strings.ReplaceAll(ref, `\`, "/"), "/")
	parts := strings.Split(text, "/")
	if ValidName(parts[0]) != nil {
		return "", "", fmt.Errorf("give a share as <name> or <name>/<subdir>, not %q", ref)
	}
	for _, p := range parts[1:] {
		if p == "" || p == "." || p == ".." || strings.Contains(p, ":") {
			return "", "", fmt.Errorf("the subdirectory in %q may not contain empty, '.' or '..' components or ':'", ref)
		}
	}
	return parts[0], strings.Join(parts[1:], "/"), nil
}
