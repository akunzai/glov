package proto

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Conn is one framed connection. Writes are serialised by a mutex, so frames
// from concurrent goroutines never interleave on the wire.
type Conn struct {
	rwc io.ReadWriteCloser
	wmu sync.Mutex
}

// NewConn wraps a byte stream.
func NewConn(rwc io.ReadWriteCloser) *Conn { return &Conn{rwc: rwc} }

// Close closes the underlying stream, which unblocks a pending read or write.
func (c *Conn) Close() error { return c.rwc.Close() }

func (c *Conn) send(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := WriteFrame(c.rwc, body); err != nil {
		return fmt.Errorf("sending a %d-byte frame: %w", len(body), err)
	}
	return nil
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// Notify sends a notification (a request without an id).
func (c *Conn) Notify(method string, params any) error {
	return c.send(notification{"2.0", method, params})
}

type response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  any            `json:"result,omitzero"`
	Error   *Error         `json:"error,omitzero"`
}

func (c *Conn) respond(id jsontext.Value, result any, perr *Error) error {
	err := c.send(response{"2.0", id, result, perr})
	if errors.Is(err, ErrFrameTooLarge) {
		err = c.send(response{"2.0", id, nil, Errorf(CodeOS, "response larger than 1 MiB")})
	}
	return err
}
