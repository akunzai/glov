package baseimage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
)

// Pruned is a release Prune removed, with the bytes it freed.
type Pruned struct {
	Release string
	Bytes   int64
}

// Prune removes the releases no VM uses: every one inUse rejects except the
// newest of each distro, which the next glov vm create would otherwise
// download again, and directories a failed Prepare left without base.vhdx.
// It also empties downloads. A Prepare under way owns the cache, so Prune
// then does nothing and reports busy; that Prepare's caller prunes after it.
// Directories whose name is no distro's are left alone.
func (c *Cache) Prune(inUse func(release string) bool) (pruned []Pruned, busy bool, err error) {
	if !c.mu.TryLock() {
		return nil, true, nil
	}
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	type release struct {
		name     string
		distro   string
		complete bool
		built    time.Time
	}
	var releases []release
	newest := map[string]release{}
	for _, e := range entries {
		distro, ok := distroOf(e.Name())
		if !e.IsDir() || !ok {
			continue
		}
		r := release{name: e.Name(), distro: distro}
		if fi, err := os.Stat(c.base(r.name).Disk()); err == nil {
			r.complete, r.built = true, fi.ModTime()
			if n, ok := newest[distro]; !ok || r.built.After(n.built) {
				newest[distro] = r
			}
		}
		releases = append(releases, r)
	}
	var errs []error
	for _, r := range releases {
		if inUse(r.name) || (r.complete && newest[r.distro].name == r.name) {
			continue
		}
		dir := c.base(r.name).Dir
		size := dirSize(dir)
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		pruned = append(pruned, Pruned{r.name, size})
	}
	// Only Prepare writes there, and it holds the lock: anything left is
	// from one that was interrupted.
	if err := os.RemoveAll(filepath.Join(c.Dir, "downloads")); err != nil {
		errs = append(errs, err)
	}
	return pruned, false, errors.Join(errs...)
}

// distroOf is the distro a release directory belongs to, from its name:
// debian-13-genericcloud-amd64-<hash>, ...
func distroOf(name string) (string, bool) {
	prefix, _, _ := strings.Cut(name, "-")
	for _, d := range api.Distros {
		if prefix == d {
			return d, true
		}
	}
	return "", false
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
