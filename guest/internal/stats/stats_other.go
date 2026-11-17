//go:build !windows && !linux

package stats

import (
	"errors"
	"time"
)

// collect is a stub that keeps the package building on other platforms.
func collect(time.Duration) (Stats, error) { return Stats{}, errors.ErrUnsupported }
