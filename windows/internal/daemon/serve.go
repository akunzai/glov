package daemon

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
)

// Serve answers connections on ln until ctx ends or ln fails.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || listenerClosed(err) {
				return ctx.Err()
			}
			return err
		}
		wg.Go(func() { d.serveConn(ctx, c) })
	}
}

func (d *Daemon) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	// The pipe's DACL already admits only SYSTEM and the owner; check the
	// caller again so a DACL mistake does not hand out the service.
	caller, err := d.callers().User(c)
	if err != nil {
		slog.Warn("control connection refused: caller unknown", "err", err)
		return
	}
	if caller != d.Owner {
		slog.Warn("control connection refused: not the owner", "caller", caller)
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A JSON-RPC frame starts with its length, whose first byte is 0 (frames
	// stay under 1 MiB), so a first byte of the SSH header tells them apart.
	r := bufio.NewReader(c)
	if b, err := r.Peek(1); err == nil && b[0] == api.SSHHeader[0] {
		d.relaySSH(ctx, caller, r, c)
		return
	}
	conn := proto.NewConn(struct {
		io.Reader
		io.WriteCloser
	}{r, c})
	if err := d.methods(ctx, caller, conn.Notify).Serve(ctx, conn); err != nil && ctx.Err() == nil {
		slog.Debug("control connection ended", "err", err)
	}
}
