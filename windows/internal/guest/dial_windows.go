// Package guest reaches a Linux VM's Guest Service over hv_sock.
package guest

import (
	"context"
	"fmt"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/akunzai/glov/windows/internal/rpc"
)

// Port is the Guest Service's vsock port. Under Hyper-V the host reaches it as
// the hv_sock service ID VsockServiceID(Port), 00000fb6-facb-11e6-bd58-64006a7986d3,
// and the guest sees the host as CID 2.
const Port = 4022

// SSHPort is the guest's SSH relay port (docs/guest-protocol.md "SSH over
// vsock"): each connection is piped to the guest's sshd.
const SSHPort = 4025

// Dial connects to the Guest Service of the VM whose HCS RuntimeId is vmID.
func Dial(ctx context.Context, vmID guid.GUID, notify rpc.NotifyFunc) (*rpc.Client, error) {
	conn, err := DialPort(ctx, vmID, Port)
	if err != nil {
		return nil, err
	}
	return rpc.New(conn, notify), nil
}

// DialPort connects to the guest's vsock port on the VM whose HCS RuntimeId
// is vmID.
//
// A connect sent while the guest is still booting, before anything listens
// on the port, neither fails nor succeeds until Windows' 30 s hv_sock connect
// timeout, and go-winio does not cancel it with ctx. DialPort gives up when
// ctx ends and lets that attempt finish in the background, so callers can
// retry with short timeouts and reach the guest as soon as it listens.
func DialPort(ctx context.Context, vmID guid.GUID, port uint32) (*winio.HvsockConn, error) {
	addr := &winio.HvsockAddr{VMID: vmID, ServiceID: winio.VsockServiceID(port)}
	type result struct {
		conn *winio.HvsockConn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := winio.Dial(context.WithoutCancel(ctx), addr)
		done <- result{conn, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("hv_sock dial %s: %w", addr, r.err)
		}
		return r.conn, nil
	case <-ctx.Done():
		go func() {
			if r := <-done; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("hv_sock dial %s: %w", addr, ctx.Err())
	}
}
