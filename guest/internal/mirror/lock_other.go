//go:build !windows

package mirror

import (
	"errors"
	"io/fs"
	"syscall"
)

// isLocked reports an error that means another process holds the file. Unix
// has no mandatory locks; a busy executable is the nearest thing.
func isLocked(err error) bool { return errors.Is(err, syscall.ETXTBSY) }

// isDenied reports an error that means access was refused.
func isDenied(err error) bool { return errors.Is(err, fs.ErrPermission) }
