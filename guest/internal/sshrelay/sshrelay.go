// Package sshrelay serves `glov ssh` for VZ guests (docs/guest-protocol.md
// "SSH over vsock"): the host connects to vsock port Port, and each connection
// is piped to the guest's sshd, so a VM without a network device is reachable
// too. The host only connects in; the relay opens nothing toward the host.
package sshrelay

import (
	"context"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/akunzai/glov/guest/internal/transport"
)

const (
	// Port is the guest's vsock port for SSH connections from the host.
	Port = 4025
	// SSHD is where the guest's sshd listens.
	SSHD     = "127.0.0.1:22"
	maxConns = 64
)

// Serve accepts host connections on l until ctx ends, piping each to a new
// connection to sshd. Past maxConns at once, a new one is closed.
func Serve(ctx context.Context, l transport.Listener, sshd string) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	defer l.Close()
	slots := make(chan struct{}, maxConns)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); relay(c, sshd) }()
		default:
			c.Close()
		}
	}
}

func relay(c io.ReadWriteCloser, sshd string) {
	defer c.Close()
	s, err := net.DialTimeout("tcp", sshd, 5*time.Second)
	if err != nil {
		slog.Warn("ssh relay: sshd unreachable", "err", err)
		return
	}
	defer s.Close()
	transport.Pipe(c, s)
}
