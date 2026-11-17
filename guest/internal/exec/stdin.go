package exec

import (
	"io"
	"sync"
)

// stdinQueue feeds a child's stdin from a goroutine so that exec.stdin never
// blocks the request loop (and with it exec.kill) on a child that is not
// reading. Order is preserved.
type stdinQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	chunks [][]byte
	eof    bool
	closed bool
}

func newStdinQueue(w io.WriteCloser) *stdinQueue {
	q := &stdinQueue{}
	q.cond = sync.NewCond(&q.mu)
	go q.run(w)
	return q
}

// push queues data and, when eof, the end of input. It reports false if the
// input was already closed.
func (q *stdinQueue) push(data []byte, eof bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.eof || q.closed {
		return false
	}
	if len(data) > 0 {
		q.chunks = append(q.chunks, data)
	}
	q.eof = eof
	q.cond.Signal()
	return true
}

// close abandons whatever is queued; used once the child is gone.
func (q *stdinQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Signal()
	q.mu.Unlock()
}

func (q *stdinQueue) run(w io.WriteCloser) {
	defer w.Close()
	broken := false
	for {
		q.mu.Lock()
		for len(q.chunks) == 0 && !q.eof && !q.closed {
			q.cond.Wait()
		}
		if q.closed || len(q.chunks) == 0 {
			q.mu.Unlock()
			return
		}
		data := q.chunks[0]
		q.chunks = q.chunks[1:]
		q.mu.Unlock()
		if !broken {
			if _, err := w.Write(data); err != nil {
				broken = true // child closed its stdin; drop the rest
			}
		}
	}
}
