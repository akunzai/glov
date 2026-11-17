package transport

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// ListenVsock listens on AF_VSOCK, any CID, the given port. The sockets are
// non-blocking and registered with the runtime poller (through os.File), so
// Close unblocks a pending Accept or Read.
func ListenVsock(port uint32) (Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, os.NewSyscallError("socket(AF_VSOCK)", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, os.NewSyscallError("bind", err)
	}
	// vsock refuses connects past the backlog outright, so a burst of host
	// connections must not meet a small one.
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		unix.Close(fd)
		return nil, os.NewSyscallError("listen", err)
	}
	return &vsockListener{f: os.NewFile(uintptr(fd), "vsock-listener")}, nil
}

type vsockListener struct{ f *os.File }

func (l *vsockListener) Accept() (io.ReadWriteCloser, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, err
	}
	for {
		var nfd int
		var peer unix.Sockaddr
		var aerr error
		err = rc.Read(func(fd uintptr) bool {
			nfd, peer, aerr = unix.Accept4(int(fd), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
			return !errors.Is(aerr, syscall.EAGAIN) // EAGAIN: wait for the poller and retry
		})
		if err != nil {
			return nil, err // the listener was closed
		}
		if aerr != nil {
			return nil, os.NewSyscallError("accept", aerr)
		}
		if !fromHost(peer) {
			slog.Warn("rejected vsock connection from a non-host peer", "peer", fmt.Sprintf("%+v", peer))
			unix.Close(nfd)
			continue
		}
		return os.NewFile(uintptr(nfd), "vsock-conn"), nil
	}
}

// fromHost reports whether the peer is the hypervisor host. Any process in the
// guest can connect to the listener (for example over the loopback CID), and a
// connection means root exec, so only CID 2 is served.
func fromHost(peer unix.Sockaddr) bool {
	vm, ok := peer.(*unix.SockaddrVM)
	return ok && vm.CID == unix.VMADDR_CID_HOST
}

func (l *vsockListener) Close() error { return l.f.Close() }

// DialVsockHost connects to the hypervisor host (CID 2) on port. The returned
// connection supports CloseWrite, so a half-close reaches the host.
func DialVsockHost(port uint32) (io.ReadWriteCloser, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, os.NewSyscallError("socket(AF_VSOCK)", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: port}); err != nil {
		unix.Close(fd)
		return nil, os.NewSyscallError("connect", err)
	}
	return &hostConn{File: os.NewFile(uintptr(fd), "vsock-host"), fd: fd}, nil
}

type hostConn struct {
	*os.File
	fd int
}

func (c *hostConn) CloseWrite() error { return unix.Shutdown(c.fd, unix.SHUT_WR) }
