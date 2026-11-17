// Package baseimage prepares the releases new VMs are created from, in
// glovd's cache directory:
//
//	<cache>\downloads\  an image while it is downloaded and converted
//	<cache>\<release>\  base.vhdx
//
// A release directory is complete once base.vhdx exists; it is written last.
// VMs' differencing disks name base.vhdx by path, so it never moves.
package baseimage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/akunzai/glov/windows/internal/cloudimage"
	"github.com/akunzai/glov/windows/internal/progress"
	"github.com/akunzai/glov/windows/internal/vhdx"
)

// Base is a prepared release.
type Base struct {
	// Release is the image name without extension, as the VM record stores
	// it, e.g. debian-13-genericcloud-amd64-3ace18e33bdd.
	Release string
	// Dir holds base.vhdx.
	Dir string
}

// Disk is the base VHDX path.
func (b Base) Disk() string { return filepath.Join(b.Dir, "base.vhdx") }

// Cache prepares releases in Dir, one at a time.
type Cache struct {
	Dir    string
	Client *http.Client

	mu sync.Mutex
}

// Prepare returns distro's newest release, downloading, verifying and
// converting it first unless it is already prepared. report receives
// lines to show the user.
func (c *Cache) Prepare(ctx context.Context, distro string, report func(string)) (Base, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	src, err := cloudimage.ByDistro(distro)
	if err != nil {
		return Base{}, err
	}
	img, err := src.Latest(ctx, c.Client)
	if err != nil {
		return Base{}, fmt.Errorf("find the newest %s image: %w", distro, err)
	}
	b := c.base(img.Release)
	if _, err := os.Stat(b.Disk()); err == nil {
		return b, nil
	}
	report("downloading " + src.File)
	qcow2, err := src.Fetch(ctx, c.Client, img, filepath.Join(c.Dir, "downloads"), report)
	if err != nil {
		return Base{}, err
	}
	if err := ctx.Err(); err != nil {
		return Base{}, err
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return Base{}, err
	}
	if err := progress.WhileRunning("converting "+src.File+" to VHDX", report, progress.Every, func() error {
		return writeDisk(b, func(dst string) error { return vhdx.FromQCOW2(qcow2, dst) })
	}); err != nil {
		return Base{}, err
	}
	os.Remove(qcow2) // only an input: base.vhdx holds the same data
	return b, nil
}

func (c *Cache) base(release string) Base {
	return Base{Release: release, Dir: filepath.Join(c.Dir, release)}
}

// writeDisk writes base.vhdx through convert, to a temporary name first, so
// base.vhdx only ever exists complete.
func writeDisk(b Base, convert func(dst string) error) error {
	part := filepath.Join(b.Dir, "base.part.vhdx")
	if err := os.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := convert(part); err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, b.Disk())
}

// Get returns the prepared release a VM was created from.
func (c *Cache) Get(release string) (Base, error) {
	if release == "" || strings.ContainsAny(release, `\/:`) || release != filepath.Base(release) {
		return Base{}, fmt.Errorf("invalid release name %q", release)
	}
	b := c.base(release)
	if _, err := os.Stat(b.Disk()); err != nil {
		return Base{}, fmt.Errorf("base image %s is missing from %s", release, c.Dir)
	}
	return b, nil
}
