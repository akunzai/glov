package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/daemon"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/service"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// dialGlovd connects to the user's glovd. Windows starts it at sign-in; if
// it is not running (it exited, or was installed after sign-in), glov
// starts it once and waits for it.
func dialGlovd(ctx context.Context, notify rpc.NotifyFunc) (*rpc.Client, error) {
	return withGlovd(ctx, func(pipe, me string) (*rpc.Client, error) { return daemon.Dial(ctx, pipe, me, notify) })
}

// dialGlovdSSH opens an SSH relay to vm through the user's glovd.
func dialGlovdSSH(ctx context.Context, vm string) (io.ReadWriteCloser, error) {
	return withGlovd(ctx, func(pipe, me string) (io.ReadWriteCloser, error) { return daemon.DialSSH(ctx, pipe, me, vm) })
}

func withGlovd[C any](ctx context.Context, dial func(pipe, me string) (C, error)) (C, error) {
	me, err := winsec.CurrentSID()
	if err != nil {
		var zero C
		return zero, err
	}
	pipe := api.Pipe(me)
	c, err := dial(pipe, me)
	if !errors.Is(err, daemon.ErrNotRunning) {
		return c, err
	}
	exited, serr := service.StartGlovd()
	if serr != nil {
		return c, err
	}
	for {
		select {
		case <-ctx.Done():
			return c, err
		case <-exited:
			// Another glov may have started it first; otherwise glovd
			// refused to start, before it could log why.
			exited = nil
			if c, err = dial(pipe, me); !errors.Is(err, daemon.ErrNotRunning) {
				return c, err
			}
			if serr := service.StartupError(me); serr != nil {
				return c, fmt.Errorf("glovd cannot start: %w", serr)
			}
			return c, fmt.Errorf("glovd exited as it started; see %s", filepath.Join(service.Home(), "glovd.log"))
		case <-time.After(200 * time.Millisecond):
		}
		if c, err = dial(pipe, me); !errors.Is(err, daemon.ErrNotRunning) {
			return c, err
		}
	}
}
