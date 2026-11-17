package proto_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/proto"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := proto.WriteFrame(&buf, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes()[:4]; !bytes.Equal(got, []byte{0, 0, 0, 7}) {
		t.Fatalf("header = %v, want big-endian length 7", got)
	}
	body, err := proto.ReadFrame(&buf)
	if err != nil || string(body) != `{"a":1}` {
		t.Fatalf("ReadFrame = %q, %v", body, err)
	}
}

func TestFrameLimit(t *testing.T) {
	if err := proto.WriteFrame(io.Discard, make([]byte, proto.MaxFrame+1)); !errors.Is(err, proto.ErrFrameTooLarge) {
		t.Fatalf("WriteFrame over 1 MiB = %v", err)
	}
	if err := proto.WriteFrame(io.Discard, make([]byte, proto.MaxFrame)); err != nil {
		t.Fatalf("WriteFrame of exactly 1 MiB = %v", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], proto.MaxFrame+1)
	if _, err := proto.ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, proto.ErrFrameTooLarge) {
		t.Fatalf("ReadFrame over 1 MiB = %v", err)
	}
}

func TestFrameTruncated(t *testing.T) {
	if _, err := proto.ReadFrame(bytes.NewReader([]byte{0, 0, 0, 5, 'a'})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated body = %v", err)
	}
	if _, err := proto.ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream = %v", err)
	}
}

// client drives a Server over an in-memory pipe.
type client struct {
	t *testing.T
	c net.Conn
}

func serve(t *testing.T, s *proto.Server) *client {
	t.Helper()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.Serve(ctx, proto.NewConn(b)); close(done) }()
	t.Cleanup(func() { cancel(); a.Close(); <-done })
	return &client{t, a}
}

func (c *client) sendRaw(body string) {
	c.t.Helper()
	if err := proto.WriteFrame(c.c, []byte(body)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) recv() string {
	c.t.Helper()
	c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, err := proto.ReadFrame(c.c)
	if err != nil {
		c.t.Fatal(err)
	}
	return string(body)
}

func (c *client) call(body string) string { c.t.Helper(); c.sendRaw(body); return c.recv() }

type addParams struct{ A, B int }
type sum struct {
	Sum int `json:"sum"`
}

func newServer() *proto.Server {
	s := proto.NewServer()
	s.Handle("add", func(p addParams) (sum, error) { return sum{p.A + p.B}, nil })
	s.Handle("nothing", func(struct{}) (struct{}, error) { return struct{}{}, nil })
	s.Handle("fail", func(struct{}) (struct{}, error) {
		return struct{}{}, proto.Errorf(proto.CodeNoUser, "nobody")
	})
	s.Handle("missing", func(struct{}) (struct{}, error) {
		return struct{}{}, errors.New("boom")
	})
	return s
}

func TestDispatch(t *testing.T) {
	c := serve(t, newServer())
	for _, tc := range []struct{ name, req, want string }{
		{"result", `{"jsonrpc":"2.0","id":7,"method":"add","params":{"A":2,"B":3}}`, `{"jsonrpc":"2.0","id":7,"result":{"sum":5}}`},
		{"string id", `{"jsonrpc":"2.0","id":"x","method":"add","params":{"A":1,"B":1}}`, `{"jsonrpc":"2.0","id":"x","result":{"sum":2}}`},
		{"no params", `{"jsonrpc":"2.0","id":1,"method":"add"}`, `{"jsonrpc":"2.0","id":1,"result":{"sum":0}}`},
		{"empty result", `{"jsonrpc":"2.0","id":1,"method":"nothing"}`, `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"fail2"}`, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found: fail2"}}`},
		{"protocol error", `{"jsonrpc":"2.0","id":1,"method":"fail"}`, `{"jsonrpc":"2.0","id":1,"error":{"code":4,"message":"nobody"}}`},
		{"plain error is os error", `{"jsonrpc":"2.0","id":1,"method":"missing"}`, `{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"boom"}}`},
		{"bad params", `{"jsonrpc":"2.0","id":1,"method":"add","params":{"A":"x"}}`, `-32602`},
		{"parse error", `{nope`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`},
		{"not an object", `[1]`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"add"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`},
	} {
		got := c.call(tc.req)
		if !strings.Contains(got, tc.want) && got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	c := serve(t, newServer())
	c.sendRaw(`{"jsonrpc":"2.0","method":"add","params":{"A":1,"B":1}}`)
	// The next reply must belong to the second request.
	if got := c.call(`{"jsonrpc":"2.0","id":2,"method":"add"}`); !strings.Contains(got, `"id":2`) {
		t.Fatalf("got %s", got)
	}
}

type sentResult struct{ ch chan<- bool }

func (sentResult) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }
func (r sentResult) Sent(delivered bool)        { r.ch <- delivered }

func TestSentRunsAfterResponse(t *testing.T) {
	order := make(chan bool, 2)
	s := proto.NewServer()
	s.Handle("go", func(struct{}) (sentResult, error) { return sentResult{order}, nil })
	c := serve(t, s)
	c.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"go"}`)
	select {
	case <-order:
		t.Fatal("Sent ran before the response was read")
	case <-time.After(50 * time.Millisecond):
	}
	c.recv()
	select {
	case delivered := <-order:
		if !delivered {
			t.Fatal("Sent(false) after a reply that was read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sent never ran")
	}
}

// A reply that cannot be written still reaches the Sender, as undelivered, so
// exec.start can end a process whose pid the host never learned.
func TestSentReportsAnUndeliveredReply(t *testing.T) {
	order := make(chan bool, 1)
	s := proto.NewServer()
	s.Handle("go", func(struct{}) (sentResult, error) { return sentResult{order}, nil })
	c := serve(t, s)
	c.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"go"}`)
	c.c.Close() // the host goes away before reading the reply
	select {
	case delivered := <-order:
		if delivered {
			t.Fatal("Sent(true) for a reply nobody read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sent never ran")
	}
}

func TestOversizedRequestDropsConnection(t *testing.T) {
	s := newServer()
	a, b := net.Pipe()
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(t.Context(), proto.NewConn(b)) }()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], proto.MaxFrame+1)
	a.Write(hdr[:])
	if err := <-errc; !errors.Is(err, proto.ErrFrameTooLarge) {
		t.Fatalf("Serve = %v, want ErrFrameTooLarge", err)
	}
}

func TestConcurrentNotificationsDoNotInterleave(t *testing.T) {
	a, b := net.Pipe()
	conn := proto.NewConn(b)
	const writers, each = 8, 50
	for i := range writers {
		go func() {
			for range each {
				conn.Notify("n", map[string]any{"i": i, "pad": strings.Repeat("x", 4096)})
			}
		}()
	}
	for range writers * each {
		a.SetReadDeadline(time.Now().Add(5 * time.Second))
		body, err := proto.ReadFrame(a)
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &v); err != nil || v.Method != "n" {
			t.Fatalf("corrupt frame: %v", err)
		}
	}
}

// failingConn is one end of a pipe whose writes over 1 KiB fail, after
// sending `cut` bytes of them (none of which reach the pipe).
type failingConn struct {
	net.Conn
	cut int
}

func (f *failingConn) Write(b []byte) (int, error) {
	if len(b) > 1024 {
		return f.cut, errors.New("insufficient system resources")
	}
	return f.Conn.Write(b)
}

// A reply that cannot be sent is logged, saying whether it was cut short.
func TestFailedReplyIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		cut        int
	}{{"nothing sent", "insufficient system resources", 0}, {"cut short", "frame cut short after 100 of", 100}} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(prev)

			s := newServer()
			s.Handle("big", func(struct{}) (string, error) { return strings.Repeat("x", 4096), nil })
			a, b := net.Pipe()
			fc := &failingConn{Conn: b, cut: tc.cut}
			done := make(chan struct{})
			go func() { s.Serve(t.Context(), proto.NewConn(fc)); close(done) }()
			t.Cleanup(func() { a.Close(); <-done })
			c := &client{t, a}

			c.sendRaw(`{"jsonrpc":"2.0","id":1,"method":"big"}`)
			// Served inline: once the next request is answered, the failed reply was logged.
			if got := c.call(`{"jsonrpc":"2.0","id":2,"method":"add"}`); !strings.Contains(got, `"id":2`) {
				t.Fatalf("got %s", got)
			}
			for _, want := range []string{`msg="reply not sent"`, "method=big", "sending a 4", tc.want} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log %q lacks %q", logs.String(), want)
				}
			}
		})
	}
}
