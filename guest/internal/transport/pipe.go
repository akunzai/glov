package transport

import (
	"io"
	"sync"
)

// Pipe copies a to b and b to a until both directions end. Each direction's
// end is passed on as a half-close where the writer supports one, else by
// closing it.
func Pipe(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(a, b)
		closeWrite(a)
	}()
	io.Copy(b, a)
	closeWrite(b)
	wg.Wait()
}

func closeWrite(c io.ReadWriteCloser) {
	if w, ok := c.(interface{ CloseWrite() error }); ok {
		w.CloseWrite()
	} else {
		c.Close()
	}
}
