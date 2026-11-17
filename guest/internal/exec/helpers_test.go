package exec

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/proto"
)

type event struct {
	method string
	params any
}

type collector struct {
	mu     sync.Mutex
	events []event
	exited map[int]chan struct{}
}

func newManager() (*Manager, *collector) {
	c := &collector{exited: map[int]chan struct{}{}}
	return NewManager(c.notify), c
}

func (c *collector) exitChan(pid int) chan struct{} {
	if c.exited[pid] == nil {
		c.exited[pid] = make(chan struct{})
	}
	return c.exited[pid]
}

func (c *collector) notify(method string, params any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o, ok := params.(outputParams); ok {
		o.Data = bytes.Clone(o.Data) // the pump reuses its buffer, as the contract says
		params = o
	}
	c.events = append(c.events, event{method, params})
	if e, ok := params.(exitParams); ok {
		close(c.exitChan(e.Pid))
	}
}

// run starts a process the way the dispatcher does and waits for its exit.
func (c *collector) run(t *testing.T, m *Manager, p StartParams) int {
	t.Helper()
	res, err := m.Start(p)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	done := c.exitChan(res.Pid)
	c.mu.Unlock()
	res.Sent(true)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("pid %d never exited", res.Pid)
	}
	return res.Pid
}

func (c *collector) forPid(pid int) (stdout, stderr []byte, methods []string, code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		switch p := e.params.(type) {
		case outputParams:
			if p.Pid != pid {
				continue
			}
			if len(p.Data) > chunkSize {
				panic("chunk over 64 KiB")
			}
			if p.Stream == "stdout" {
				stdout = append(stdout, p.Data...)
			} else {
				stderr = append(stderr, p.Data...)
			}
		case exitParams:
			if p.Pid != pid {
				continue
			}
			code = p.Code
		}
		if pidOf(e.params) == pid {
			methods = append(methods, e.method)
		}
	}
	return
}

func pidOf(p any) int {
	switch v := p.(type) {
	case outputParams:
		return v.Pid
	case exitParams:
		return v.Pid
	}
	return -1
}

func errCode(err error) int {
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return pe.Code
	}
	return 0
}
