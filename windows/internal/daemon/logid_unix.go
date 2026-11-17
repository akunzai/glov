//go:build unix

package daemon

import (
	"io/fs"
	"strconv"
	"syscall"
)

// logID tells one boot's console log from the next: its inode, since every
// boot creates the file anew.
func logID(fi fs.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return strconv.FormatUint(uint64(st.Ino), 10)
	}
	return ""
}
