package guestservice

import (
	"sync"

	"github.com/akunzai/glov/guest/proto"
)

// hub routes notifications to whichever connection is current. A notification
// issued while the port is down, or whose write fails, waits for the next
// connection and is written there in full, so process output and exits are
// neither lost nor torn across a host restart.
type hub struct {
	mu   sync.Mutex
	cur  *proto.Conn
	up   chan struct{} // closed while cur != nil
	dead chan struct{} // closed on shutdown
}

func newHub() *hub { return &hub{up: make(chan struct{}), dead: make(chan struct{})} }

func (h *hub) set(c *proto.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = c
	close(h.up)
}

func (h *hub) drop(c *proto.Conn) {
	h.mu.Lock()
	if h.cur == c {
		h.cur = nil
		h.up = make(chan struct{})
	}
	h.mu.Unlock()
	c.Close()
}

func (h *hub) shutdown() { close(h.dead) }

func (h *hub) notify(method string, params any) {
	for {
		h.mu.Lock()
		c, up := h.cur, h.up
		h.mu.Unlock()
		if c == nil {
			select {
			case <-up:
				continue
			case <-h.dead:
				return
			}
		}
		if c.Notify(method, params) == nil {
			return
		}
		h.drop(c)
	}
}
