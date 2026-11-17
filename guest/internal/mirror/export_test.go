package mirror

import (
	"io"
	"os"
	"time"
)

// SetOpenForHash replaces how files are opened for hashing until the test ends.
func SetOpenForHash(open func(r *os.Root, rel string) (io.ReadCloser, error)) (restore func()) {
	old := openForHash
	openForHash = open
	return func() { openForHash = old }
}

// SetLockWait shortens how long a call waits for the share's previous one.
func SetLockWait(d time.Duration) (restore func()) {
	old := lockWait
	lockWait = d
	return func() { lockWait = old }
}
