// Package transport opens the byte stream the protocol runs over.
package transport

import (
	"io"
	"os"
)

// WindowsPort is the virtio-serial port the host exposes as guest.sock.
const WindowsPort = `\\.\Global\org.glov.guest.0`

type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error { return os.Stdin.Close() }

// Stdio serves the protocol over stdin and stdout, for tests and manual runs.
func Stdio() io.ReadWriteCloser { return stdio{os.Stdin, os.Stdout} }

// VsockPort is the AF_VSOCK port the Linux Guest Service listens on; the host connects.
const VsockPort = 4022

// Listener hands out one stream per host connection. Close unblocks a pending
// Accept, which then fails.
type Listener interface {
	Accept() (io.ReadWriteCloser, error)
	Close() error
}
