package mirror

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func isReparsePoint(fi fs.FileInfo) bool {
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return false
}

// checkOpened checks that r is the directory at the path at, by asking
// Windows for the final path of the opened handle: through a junction or
// symbolic link it is the target's path instead.
func checkOpened(r *os.Root, _ fs.FileInfo, at string) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 0 /* VOLUME_NAME_DOS */)
	if err != nil {
		return &os.PathError{Op: "GetFinalPathNameByHandle", Path: at, Err: err}
	}
	final := strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`)
	if !strings.EqualFold(filepath.Clean(final), filepath.Clean(at)) {
		return fmt.Errorf("%s %w (it resolves to %s)", at, errLinkedRoot, final)
	}
	return nil
}
