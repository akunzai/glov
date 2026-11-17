package transport

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// OpenPort opens a virtio-serial port (or any device or named pipe path) for
// overlapped I/O. A synchronous handle serialises operations, so a pending
// read would block every write; overlapped lets both directions run at once.
func OpenPort(path string) (io.ReadWriteCloser, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	f := &port{h: h}
	for _, ev := range []*windows.Handle{&f.rev, &f.wev} {
		if *ev, err = windows.CreateEvent(nil, 1, 0, nil); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// port allows one read and one write in flight at a time, which is all the
// protocol needs (one reader loop, writes under the Conn mutex).
type port struct {
	h        windows.Handle
	rev, wev windows.Handle
	rov, wov windows.Overlapped // live in the heap-allocated port: the kernel keeps them while I/O is pending
	rmu, wmu sync.Mutex         // one op per direction
	closed   atomic.Bool
}

func (p *port) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n, err := p.do(&p.rmu, &p.rov, p.rev, b, windows.ReadFile)
	if err == nil && n == 0 {
		return 0, io.EOF
	}
	if err == windows.ERROR_BROKEN_PIPE || err == windows.ERROR_HANDLE_EOF {
		err = io.EOF
	}
	return n, err
}

func (p *port) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := p.do(&p.wmu, &p.wov, p.wev, b[total:], windows.WriteFile)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (p *port) do(mu *sync.Mutex, ov *windows.Overlapped, ev windows.Handle, b []byte,
	op func(windows.Handle, []byte, *uint32, *windows.Overlapped) error) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	if p.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	*ov = windows.Overlapped{HEvent: ev}
	windows.ResetEvent(ev)
	var n uint32
	err := op(p.h, b, &n, ov)
	if err == windows.ERROR_IO_PENDING {
		err = windows.GetOverlappedResult(p.h, ov, &n, true)
	}
	if err != nil {
		return int(n), err
	}
	return int(n), nil
}

// Close cancels pending I/O and releases the handle. It is safe to call twice.
func (p *port) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	// An op that slipped past the closed check may issue its I/O after the
	// first cancel, so keep cancelling until both directions are idle.
	for _, mu := range []*sync.Mutex{&p.rmu, &p.wmu} {
		for !mu.TryLock() {
			windows.CancelIoEx(p.h, nil)
			time.Sleep(time.Millisecond)
		}
		defer mu.Unlock()
	}
	for _, h := range []windows.Handle{p.rev, p.wev} {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}
	return windows.CloseHandle(p.h)
}
