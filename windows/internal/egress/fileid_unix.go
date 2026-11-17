//go:build unix

package egress

import (
	"errors"
	"os"
	"syscall"
)

// fileID is f's inode, which a rename keeps, as the NTFS file index does on
// Windows.
func fileID(f *os.File) (uint64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no inode for " + f.Name())
	}
	return uint64(st.Ino), nil
}
