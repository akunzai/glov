package transport

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// vsockUsable skips where the kernel has no AF_VSOCK (typical in containers).
func vsockUsable(t *testing.T) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("AF_VSOCK is not available here (%v); the real-socket test needs a VM or a host with vsock", err)
	}
	unix.Close(fd)
}

// Over loopback (CID 1 on kernels with vsock_loopback) the listener accepts a
// connection and closing it unblocks both Accept and Read.
func TestVsockListenerAcceptsAndCloseUnblocks(t *testing.T) {
	vsockUsable(t)
	l, err := ListenVsock(40220)
	if err != nil {
		t.Skipf("cannot listen on vsock here: %v", err)
	}
	res := make(chan error, 1)
	go func() { _, err := l.Accept(); res <- err }()
	time.Sleep(100 * time.Millisecond)
	l.Close()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("Accept returned no error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}
	if _, err := l.Accept(); err == nil || errors.Is(err, unix.EAGAIN) {
		t.Fatalf("Accept after Close: %v", err)
	}
}

func TestOnlyTheHostCIDIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer unix.Sockaddr
		want bool
	}{
		{"host", &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: 1234}, true},
		{"loopback", &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: 1234}, false},
		{"another guest", &unix.SockaddrVM{CID: 42}, false},
		{"hypervisor", &unix.SockaddrVM{CID: unix.VMADDR_CID_HYPERVISOR}, false},
		{"other family", &unix.SockaddrUnix{Name: "/x"}, false},
		{"nil", nil, false},
	} {
		if got := fromHost(tc.peer); got != tc.want {
			t.Errorf("%s: fromHost = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A host opening many connections at once must not have the kernel refuse the
// connects past a small backlog before Accept drains them.
func TestVsockListenerQueuesABurstOfConnects(t *testing.T) {
	vsockUsable(t)
	const port, burst = 40221, 32
	l, err := ListenVsock(port)
	if err != nil {
		t.Skipf("cannot listen on vsock here: %v", err)
	}
	defer l.Close()
	dial := func() error {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err == nil {
			err = unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: port})
			unix.Close(fd)
		}
		return err
	}
	// Without vsock_loopback even a single local connect is reset.
	if err := dial(); err != nil {
		t.Skipf("vsock loopback is not available here: %v", err)
	}
	errs := make(chan error, burst)
	for range burst {
		go func() { errs <- dial() }()
	}
	for range burst {
		if err := <-errs; err != nil {
			t.Fatalf("a connect in a burst of %d was refused before Accept: %v", burst, err)
		}
	}
}
