package sshrelay

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// tcpListener stands in for the vsock listener.
type tcpListener struct{ net.Listener }

func (l tcpListener) Accept() (io.ReadWriteCloser, error) { return l.Listener.Accept() }

// TestServeRelaysEachHostConnectionToSSHD checks bytes go both ways and a
// half-close reaches sshd, which still answers after it.
func TestServeRelaysEachHostConnectionToSSHD(t *testing.T) {
	sshd, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sshd.Close()
	go func() {
		for {
			c, err := sshd.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				got, _ := io.ReadAll(c) // until the host's half-close
				c.Write(append([]byte("SSH-2.0-test "), got...))
			}()
		}
	}()
	vsock, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, tcpListener{vsock}, sshd.Addr().String()) }()

	for _, msg := range []string{"first", "second"} {
		c, err := net.Dial("tcp", vsock.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte(msg))
		c.(*net.TCPConn).CloseWrite()
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "SSH-2.0-test "+msg {
			t.Fatalf("got %q, %v", got, err)
		}
		c.Close()
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

// TestNoSSHDClosesTheHostConnection checks a guest without sshd closes the
// host's connection instead of holding it.
func TestNoSSHDClosesTheHostConnection(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	closed.Close()
	vsock, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, tcpListener{vsock}, addr)
	c, err := net.Dial("tcp", vsock.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if got, err := io.ReadAll(c); err != nil || len(got) != 0 {
		t.Fatalf("got %q, %v", got, err)
	}
}
