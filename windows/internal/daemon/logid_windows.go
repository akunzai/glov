package daemon

import (
	"io/fs"
	"strconv"
	"syscall"
)

// logID tells one boot's console log from the next: its creation time,
// since every boot creates the file anew.
func logID(fi fs.FileInfo) string {
	if a, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return strconv.FormatInt(a.CreationTime.Nanoseconds(), 10)
	}
	return ""
}
