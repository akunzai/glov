//go:build !linux

package main

import (
	"context"
	"io"

	"github.com/akunzai/glov/guest/internal/guestservice"
	"github.com/akunzai/glov/guest/internal/transport"
)

// serveGuest talks over the virtio-serial port the host exposes (Windows).
func serveGuest(ctx context.Context, a *guestservice.Service) {
	a.Run(ctx, func() (io.ReadWriteCloser, error) { return transport.OpenPort(transport.WindowsPort) }, retryEvery)
}
