package vmrun

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/akunzai/glov/windows/internal/guest"
	"github.com/akunzai/glov/windows/internal/rpc"
)

// guest is the running VM id's Guest Service connection and boot.
func (m *Manager) guest(id string) (*rpc.Client, string, error) {
	r, ok := m.get(id)
	if !ok {
		return nil, "", ErrNotRunning
	}
	return r.session.guest()
}

func (m *Manager) jobsOf(id string) (*jobs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNoJob
	}
	return j, nil
}

// ExecStart starts p in the VM's guest and returns the job's ID.
func (m *Manager) ExecStart(ctx context.Context, id string, p ExecParams) (string, error) {
	c, bootID, err := m.guest(id)
	if err != nil {
		return "", err
	}
	j, err := m.jobsOf(id)
	if err != nil {
		return "", err
	}
	var res struct {
		PID int `json:"pid"`
	}
	if err := c.Call(ctx, "exec.start", p, &res); err != nil {
		return "", err
	}
	return j.started(res.PID, bootID), nil
}

// ExecRead returns a job's output from cursor on, at most max bytes.
func (m *Manager) ExecRead(id, job string, cursor, max int) (Read, error) {
	j, err := m.jobsOf(id)
	if err != nil {
		return Read{}, err
	}
	return j.read(job, cursor, max)
}

// ExecKill ends a running job's process tree.
func (m *Manager) ExecKill(ctx context.Context, id, job string) error {
	j, err := m.jobsOf(id)
	if err != nil {
		return err
	}
	pid, err := j.pid(job)
	if err != nil {
		return err
	}
	c, _, err := m.guest(id)
	if err != nil {
		return err
	}
	return c.Call(ctx, "exec.kill", map[string]int{"pid": pid}, nil)
}

// GuestCall passes one of FileMethods or SyncMethods, or guest.stats for
// vm.stats, or secrets.sync for a harness's tmpfs secrets, to the VM's Guest
// Service.
func (m *Manager) GuestCall(ctx context.Context, id, method string, params, result any) error {
	if !FileMethods[method] && !SyncMethods[method] && method != "guest.stats" && method != "secrets.sync" {
		return errors.New("not a Guest Service method glovd passes on: " + method)
	}
	c, _, err := m.guest(id)
	if err != nil {
		return err
	}
	return c.Call(ctx, method, params, result)
}

// DialSSH connects to the running VM id's SSH relay, which pipes the
// connection to the guest's sshd.
func (m *Manager) DialSSH(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	return m.Dial(ctx, id, guest.SSHPort)
}

// Dial connects to vsock port on the running VM id, giving up after 10 s.
func (m *Manager) Dial(ctx context.Context, id string, port uint32) (net.Conn, error) {
	if !m.Running(id) {
		return nil, ErrNotRunning
	}
	vmID, err := guid.FromString(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return guest.DialPort(ctx, vmID, port)
}
