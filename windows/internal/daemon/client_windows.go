package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// Dial connects glov to glovd at path. It connects at identification
// level, so the server learns who calls but cannot act as them, and refuses
// a pipe not owned by wantOwner (SYSTEM for the installed service). notify,
// if not nil, receives glovd's notifications (api.NotifyProgress).
func Dial(ctx context.Context, path, wantOwner string, notify rpc.NotifyFunc) (*rpc.Client, error) {
	c, err := open(ctx, path, wantOwner)
	if err != nil {
		return nil, err
	}
	return handshake(ctx, c, notify)
}

// open connects to the pipe at path at identification level, so the server
// learns who calls but cannot act as them, and refuses a pipe not owned by
// wantOwner.
func open(ctx context.Context, path, wantOwner string) (net.Conn, error) {
	const genericRead, genericWrite = 0x80000000, 0x40000000
	c, err := winio.DialPipeAccessImpLevel(ctx, path, genericRead|genericWrite, winio.PipeImpLevelIdentification)
	if err != nil {
		if errors.Is(err, winio.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) || isNotFound(err) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("connect to glovd: %w", err)
	}
	owner, err := winsec.PipeOwner(c)
	if err == nil && owner != wantOwner {
		err = fmt.Errorf("%s is owned by %s, not %s: another program holds glovd's pipe", path, owner, wantOwner)
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// DialSSH opens an SSH relay to vm's sshd through glovd at path (see
// api.SSHHeader); the returned connection carries ssh's bytes.
func DialSSH(ctx context.Context, path, wantOwner, vm string) (io.ReadWriteCloser, error) {
	c, err := open(ctx, path, wantOwner)
	if err != nil {
		return nil, err
	}
	return sshRelay(ctx, c, vm)
}

func isNotFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
