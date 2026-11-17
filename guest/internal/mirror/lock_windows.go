package mirror

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isLocked reports an error that means another process holds the file.
func isLocked(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// isDenied reports an error that means access was refused. Replacing or
// deleting a file another process holds without FILE_SHARE_DELETE fails with
// it too, so for those operations it may also mean in use.
func isDenied(err error) bool { return errors.Is(err, windows.ERROR_ACCESS_DENIED) }
