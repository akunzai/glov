package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/rpc"
)

// ErrNotRunning reports that no glovd answers on the pipe.
var ErrNotRunning = errors.New("glovd is not running (glov host install sets it up)")

// handshake checks that glovd on c speaks this glov's protocol and
// returns the client; it closes c on error.
func handshake(ctx context.Context, c net.Conn, notify rpc.NotifyFunc) (*rpc.Client, error) {
	client := rpc.New(c, notify)
	var info api.Info
	if err := client.Call(ctx, api.MethodInfo, nil, &info); err != nil {
		client.Close()
		return nil, fmt.Errorf("glovd: %w", err)
	}
	if info.ProtocolVersion != api.ProtocolVersion {
		client.Close()
		return nil, fmt.Errorf("glovd %s speaks protocol %d, this glov %d: run glov host install from the same build", info.Version, info.ProtocolVersion, api.ProtocolVersion)
	}
	return client, nil
}

// sshRelay asks glovd on c for an SSH relay to vm's sshd; it closes c
// on error.
func sshRelay(ctx context.Context, c net.Conn, vm string) (io.ReadWriteCloser, error) {
	header := api.SSHHeader + vm + "\n"
	if len(header) > api.SSHHeaderMax || strings.ContainsAny(vm, "\r\n") {
		c.Close()
		return nil, fmt.Errorf("invalid VM name %q", vm)
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	r := bufio.NewReader(c)
	answer, err := writeAndReadLine(c, r, header)
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("glovd: %w", err)
	}
	if reason, ok := strings.CutPrefix(answer, "error: "); ok {
		c.Close()
		return nil, errors.New(reason)
	}
	if answer != "ok" {
		c.Close()
		return nil, fmt.Errorf("glovd answered %q", answer)
	}
	return struct {
		io.Reader
		io.WriteCloser
	}{r, c}, nil
}

func writeAndReadLine(w io.Writer, r *bufio.Reader, line string) (string, error) {
	if _, err := io.WriteString(w, line); err != nil {
		return "", err
	}
	answer, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(answer, "\n"), nil
}
