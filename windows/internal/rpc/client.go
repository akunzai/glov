// Package rpc is the calling side of glov's framed JSON-RPC 2.0 (docs/guest-protocol.md),
// used by the host towards a Guest Service and by glov towards glovd.
// A Client sends requests over one connection and delivers the peer's notifications.
package rpc

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/akunzai/glov/guest/proto"
)

// ErrClosed reports a call on a connection that has ended.
var ErrClosed = errors.New("rpc: connection closed")

// NotifyFunc receives a notification from the peer. It runs on the read loop,
// so notifications arrive in wire order; it must return quickly.
type NotifyFunc func(method string, params jsontext.Value)

// Client is one connection to a peer. Calls may run concurrently.
type Client struct {
	rwc    io.ReadWriteCloser
	notify NotifyFunc

	wmu sync.Mutex // serialises frames on the wire

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	err     error // set once the read loop ends

	done chan struct{}
}

type reply struct {
	result jsontext.Value
	err    error
}

// New starts reading rwc. notify may be nil.
func New(rwc io.ReadWriteCloser, notify NotifyFunc) *Client {
	if notify == nil {
		notify = func(string, jsontext.Value) {}
	}
	c := &Client{rwc: rwc, notify: notify, pending: map[int64]chan reply{}, done: make(chan struct{})}
	go c.readLoop()
	return c
}

// Close ends the connection; pending calls fail with ErrClosed.
func (c *Client) Close() error { return c.rwc.Close() }

// Done is closed when the connection has ended; Err then says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the error that ended the connection, or nil while it is up.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitzero"`
}

// Call sends method with params (nil for none) and decodes the result into
// result (nil to discard it). A peer error is returned as *proto.Error.
// If ctx ends first the call returns ctx.Err(); a late answer is logged and dropped.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrClosed, c.err)
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	body, err := json.Marshal(request{"2.0", id, method, params})
	if err != nil {
		return err
	}
	c.wmu.Lock()
	err = proto.WriteFrame(c.rwc, body)
	c.wmu.Unlock()
	if err != nil {
		if errors.Is(err, proto.ErrFrameCut) {
			// The peer now reads garbage; only a new connection recovers.
			c.Close()
		}
		return fmt.Errorf("%s: %w", method, err)
	}

	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(r.result, result); err != nil {
			return fmt.Errorf("%s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// message is any frame the peer sends: a response or a notification.
type message struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  *proto.Error   `json:"error"`
}

func (c *Client) readLoop() {
	var err error
	for {
		var body []byte
		if body, err = proto.ReadFrame(c.rwc); err != nil {
			break
		}
		var m message
		if jerr := json.Unmarshal(body, &m); jerr != nil {
			slog.Warn("rpc frame is not JSON", "size", len(body), "err", jerr)
			continue
		}
		if m.Method != "" && len(m.ID) == 0 {
			c.notify(m.Method, m.Params)
			continue
		}
		var id int64
		if jerr := json.Unmarshal(m.ID, &id); jerr != nil {
			slog.Warn("rpc response has no usable id", "id", string(m.ID), "error", m.Error)
			continue
		}
		// Take the entry out before delivering: each id gets at most one reply,
		// so the 1-slot channel never blocks, even if the peer answers twice.
		c.mu.Lock()
		ch, ok := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if !ok {
			slog.Info("rpc answer for a request that no longer waits", "id", id)
			continue
		}
		r := reply{result: m.Result}
		if m.Error != nil {
			r.err = m.Error
		}
		ch <- r
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) {
		err = ErrClosed
	}
	c.mu.Lock()
	c.err = err
	for id, ch := range c.pending { // still pending, so nothing was delivered yet
		ch <- reply{err: fmt.Errorf("%w: %w", ErrClosed, err)}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.rwc.Close()
	close(c.done)
}
