//go:build !windows

package logfile

import (
	"fmt"
	"os"
)

// PrepareDir creates dir if needed and refuses a symlink in its place.
func PrepareDir(dir string) error {
	if fi, err := os.Lstat(dir); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (%v)", dir, fi.Mode().Type())
	}
	return os.MkdirAll(dir, 0o755)
}
