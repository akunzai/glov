package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

// readHeader reads the relay's header line, at most api.SSHHeaderMax bytes
// with its newline, and returns the VM named in it.
func readHeader(r *bufio.Reader) (string, error) {
	var line []byte
	for len(line) < api.SSHHeaderMax {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			vm, ok := strings.CutPrefix(string(line), api.SSHHeader)
			if !ok || vm == "" {
				return "", errors.New("malformed header")
			}
			return vm, nil
		}
		line = append(line, b)
	}
	return "", errors.New("header too long")
}

// relaySSH serves one `glov ssh-relay` connection (docs/guest-protocol.md
// "SSH over vsock"): it reads `glov-ssh <vm>`, answers `ok` once the
// caller's VM's SSH relay answers, or `error: <reason>`, then pipes bytes
// both ways until either side closes.
func (d *Daemon) relaySSH(ctx context.Context, caller string, r *bufio.Reader, c io.ReadWriteCloser) {
	vm, err := readHeader(r)
	if err != nil {
		fmt.Fprintf(c, "error: %v\n", err)
		return
	}
	guestConn, err := d.dialSSH(ctx, caller, vm)
	if err != nil {
		fmt.Fprintf(c, "error: %v\n", err)
		return
	}
	defer guestConn.Close()
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { c.Close(); guestConn.Close() })
	defer stop()
	pipe(struct {
		io.Reader
		io.WriteCloser
	}{r, c}, guestConn)
}

func (d *Daemon) dialSSH(ctx context.Context, caller, ref string) (io.ReadWriteCloser, error) {
	rec, err := d.load(caller, ref)
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return nil, errors.New(pe.Message)
	}
	if err != nil {
		return nil, err
	}
	conn, err := d.VMs.DialSSH(ctx, rec.ID)
	if errors.Is(err, vmrun.ErrNotRunning) {
		return nil, fmt.Errorf("VM %s is not running: glov vm start %s", rec.Name, rec.Name)
	}
	if err != nil {
		slog.Warn("ssh relay", "vm", rec.Name, "err", err)
		return nil, fmt.Errorf("VM %s: the guest's SSH relay does not answer (is the Guest Service ready? glov vm wait %s guest-ready)", rec.Name, rec.Name)
	}
	return conn, nil
}

// pipe copies a to b and b to a until both directions end; when one
// direction ends, both ends are closed, so the other stops too.
func pipe(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() { a.Close(); b.Close() }
	var wg sync.WaitGroup
	wg.Go(func() { io.Copy(a, b); once.Do(closeBoth) })
	wg.Go(func() { io.Copy(b, a); once.Do(closeBoth) })
	wg.Wait()
}
