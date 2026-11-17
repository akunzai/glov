package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/akunzai/glov/guest/proto"
)

// prepare mounts a tmpfs of its own on dir unless one is there: root-owned
// 0711, so the owner opens its files by name but nobody else lists them, and
// noswap, so the pages never reach a swap device either.
func prepare(dir string) error {
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return proto.FromOS(err)
	}
	mounted, err := isMountPoint(dir)
	if err != nil {
		return proto.FromOS(err)
	}
	if !mounted {
		flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
		err := unix.Mount("glov-secrets", dir, "tmpfs", flags, "mode=0711,size=4m,noswap")
		if errors.Is(err, unix.EINVAL) {
			// Kernels before 6.4 have no noswap; an isolated VM has no swap unless one is added.
			err = unix.Mount("glov-secrets", dir, "tmpfs", flags, "mode=0711,size=4m")
		}
		if err != nil {
			return proto.Errorf(proto.CodeOS, "mount a tmpfs on %s: %v", dir, err)
		}
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return proto.FromOS(err)
	}
	if st.Type != unix.TMPFS_MAGIC {
		return proto.Errorf(proto.CodeOS, "%s is not a tmpfs (filesystem type %#x); refusing to write secrets there", dir, st.Type)
	}
	return nil
}

func isMountPoint(dir string) (bool, error) {
	var a, b unix.Stat_t
	if err := unix.Lstat(dir, &a); err != nil {
		return false, err
	}
	if a.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	if err := unix.Lstat(filepath.Dir(dir), &b); err != nil {
		return false, err
	}
	return a.Dev != b.Dev, nil
}
