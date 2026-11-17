package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/akunzai/glov/guest/internal/egress"
	"github.com/akunzai/glov/guest/internal/guestservice"
	"github.com/akunzai/glov/guest/internal/hostclock"
	"github.com/akunzai/glov/guest/internal/sshrelay"
	"github.com/akunzai/glov/guest/internal/transport"
)

// serveGuest listens on vsock for the host, which connects to the guest. The
// vsock transport may not be loaded yet early in boot, so listening is retried.
func serveGuest(ctx context.Context, a *guestservice.Service) {
	go serveSSHRelay(ctx)
	// Only under Hyper-V (a Windows host); elsewhere it returns at once.
	go hostclock.Watch(ctx)
	if egress.Enabled() {
		// Before port 4022 serves anything, so no exec starts without the proxy.
		for k, v := range egress.Env() {
			os.Setenv(k, v)
		}
		go serveEgress(ctx)
	}
	var lastErr string
	for {
		l, err := transport.ListenVsock(transport.VsockPort)
		if err == nil {
			slog.Info("listening", "vsockPort", transport.VsockPort)
			a.RunListener(ctx, l, retryEvery)
			return
		}
		if msg := err.Error(); msg != lastErr {
			slog.Info("vsock not available", "err", msg)
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryEvery):
		}
	}
}

// serveEgress runs the isolated VM's in-guest proxy listener, which pipes each
// client to the host's egress proxy over vsock.
func serveEgress(ctx context.Context) {
	dial := func() (io.ReadWriteCloser, error) { return transport.DialVsockHost(egress.HostPort) }
	for ctx.Err() == nil {
		l, err := net.Listen("tcp", egress.Addr)
		if err == nil {
			slog.Info("egress proxy listening", "addr", egress.Addr, "vsockPort", egress.HostPort)
			err = egress.Serve(ctx, l, dial)
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("egress proxy unavailable", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryEvery):
		}
	}
}

// serveSSHRelay pipes the host's vsock connections for `glov ssh` to sshd,
// retrying while vsock is not available yet.
func serveSSHRelay(ctx context.Context) {
	for ctx.Err() == nil {
		l, err := transport.ListenVsock(sshrelay.Port)
		if err == nil {
			slog.Info("ssh relay listening", "vsockPort", sshrelay.Port, "sshd", sshrelay.SSHD)
			err = sshrelay.Serve(ctx, l, sshrelay.SSHD)
		}
		if err != nil && ctx.Err() == nil {
			slog.Debug("ssh relay unavailable", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryEvery):
		}
	}
}
