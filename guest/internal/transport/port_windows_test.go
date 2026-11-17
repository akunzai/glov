package transport

import (
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// pipeServer makes a one-instance named pipe and returns its path and a
// synchronous server handle, connected once a client opens the path.
func pipeServer(t *testing.T) (string, windows.Handle) {
	t.Helper()
	path := fmt.Sprintf(`\\.\pipe\glov-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	go windows.ConnectNamedPipe(h, nil)
	return path, h
}

// A pending read must not block a write on the same handle: with a
// synchronous handle it would, and the Guest Service could never answer a request
// while waiting for the next one.
func TestPortReadsAndWritesConcurrently(t *testing.T) {
	path, srv := pipeServer(t)
	port, err := OpenPort(path)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()

	go func() {
		buf := make([]byte, 16)
		var n uint32
		if windows.ReadFile(srv, buf, &n, nil) == nil {
			windows.WriteFile(srv, append([]byte("pong:"), buf[:n]...), &n, nil)
		}
	}()

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := port.Read(buf) // pending before the write below
		got <- string(buf[:n])
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := port.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != "pong:ping" {
			t.Fatalf("read %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read never completed: write was blocked behind the pending read")
	}
}

func TestPortCloseUnblocksPendingRead(t *testing.T) {
	path, _ := pipeServer(t)
	port, err := OpenPort(path)
	if err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { _, err := port.Read(make([]byte, 8)); res <- err }()
	time.Sleep(200 * time.Millisecond)
	port.Close()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("read returned no error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock the read")
	}
	if err := port.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpenMissingPortFails(t *testing.T) {
	if p, err := OpenPort(`\\.\Global\org.glov.nonexistent`); err == nil {
		p.Close()
		t.Fatal("opened a port that does not exist")
	}
}
