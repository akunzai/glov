package mirror

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// errLinkedRoot means the share folder, or a folder above it, is a symbolic
// link or junction. The Guest Service runs as LocalSystem (root): following one would
// let a guest process that can replace C:\work\proj with a junction make the
// sync list, write and delete wherever the junction points.
var errLinkedRoot = errors.New("is or lies below a symbolic link or junction; refusing to sync through it")

// openShareRoot opens root one component at a time from the volume root, each
// relative to the handle of its parent, refusing any component that is a link
// and checking that the directory opened is the one inspected. With create,
// missing components are made in their parent. Without it, a missing
// component yields an fs.ErrNotExist error.
func openShareRoot(root string, create bool) (*os.Root, error) {
	root = filepath.Clean(root)
	vol := filepath.VolumeName(root) + string(filepath.Separator)
	r, err := os.OpenRoot(vol)
	if err != nil {
		return nil, err
	}
	at := vol
	for name := range strings.SplitSeq(strings.TrimPrefix(root, vol), string(filepath.Separator)) {
		if name == "" {
			continue
		}
		at = filepath.Join(at, name)
		next, err := openChild(r, name, at, create)
		r.Close()
		if err != nil {
			return nil, err
		}
		r = next
	}
	return r, nil
}

func openChild(parent *os.Root, name, at string, create bool) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	if create && errors.Is(err, fs.ErrNotExist) {
		if err = parent.Mkdir(name, 0o755); err == nil || errors.Is(err, fs.ErrExist) {
			fi, err = parent.Lstat(name)
		}
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 || isReparsePoint(fi) {
		return nil, fmt.Errorf("%s %w", at, errLinkedRoot)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", at)
	}
	r, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	// The name may have been swapped for a link between Lstat and OpenRoot.
	if err := checkOpened(r, fi, at); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
