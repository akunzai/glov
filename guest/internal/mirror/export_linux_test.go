package mirror

import "time"

// SetDebounce shortens how long a watch waits before reporting changes.
func SetDebounce(s, limit time.Duration) (restore func()) {
	oldS, oldMax := settle, maxDelay
	settle, maxDelay = s, limit
	return func() { settle, maxDelay = oldS, oldMax }
}
