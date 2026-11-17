//go:build unix

package mirror

import (
	"fmt"
	"io/fs"
	"os"
)

func isReparsePoint(fs.FileInfo) bool { return false }

// checkOpened checks that r is the directory Lstat reported as fi.
func checkOpened(r *os.Root, fi fs.FileInfo, at string) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(st, fi) {
		return fmt.Errorf("%s %w", at, errLinkedRoot)
	}
	return nil
}
