//go:build !windows

package transport

import (
	"errors"
	"io"
)

// OpenPort is only implemented on Windows; Linux will use vsock (M4).
func OpenPort(string) (io.ReadWriteCloser, error) { return nil, errors.ErrUnsupported }
