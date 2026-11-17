package rpc

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/proto"
)

type fakeRequest struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
}

// fakeGuest serves the guest end of a pipe with handle until the pipe closes.
func fakeGuest(t *testing.T, conn net.Conn, handle func(fakeRequest) []any) {
	t.Helper()
	go func() {
		for {
			body, err := proto.ReadFrame(conn)
			if err != nil {
				return
			}
			var req fakeRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("request is not JSON: %s", body)
				return
			}
			for _, m := range handle(req) {
				out, _ := json.Marshal(m)
				if proto.WriteFrame(conn, out) != nil {
					return
				}
			}
		}
	}()
}

func newPair(t *testing.T, handle func(fakeRequest) []any, notify NotifyFunc) *Client {
	host, guest := net.Pipe()
	fakeGuest(t, guest, handle)
	c := New(host, notify)
	t.Cleanup(func() { c.Close(); guest.Close() })
	return c
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestCallDecodesResult(t *testing.T) {
	c := newPair(t, func(r fakeRequest) []any {
		if r.Method != "guest.info" || len(r.Params) != 0 {
			t.Errorf("request = %+v, want guest.info without params", r)
		}
		return []any{map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": map[string]any{"protocolVersion": 1, "os": "linux"}}}
	}, nil)
	var info struct {
		ProtocolVersion int    `json:"protocolVersion"`
		OS              string `json:"os"`
	}
	if err := c.Call(ctx(t), "guest.info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != 1 || info.OS != "linux" {
		t.Fatalf("info = %+v", info)
	}
}

func TestCallReturnsGuestError(t *testing.T) {
	c := newPair(t, func(r fakeRequest) []any {
		return []any{map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": 2, "message": "no such file"}}}
	}, nil)
	err := c.Call(ctx(t), "file.stat", map[string]string{"path": "/x"}, nil)
	e, ok := errors.AsType[*proto.Error](err)
	if !ok || e.Code != 2 || e.Message != "no such file" {
		t.Fatalf("err = %v, want guest error 2", err)
	}
}

func TestNotificationsArriveInOrderBeforeTheResponse(t *testing.T) {
	var got []string
	c := newPair(t, func(r fakeRequest) []any {
		return []any{
			map[string]any{"jsonrpc": "2.0", "method": "exec.output", "params": map[string]any{"pid": 7, "data": "YQ=="}},
			"not an object",
			map[string]any{"jsonrpc": "2.0", "method": "exec.exit", "params": map[string]any{"pid": 7, "code": 0}},
			map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": map[string]any{"pid": 7}},
		}
	}, func(method string, _ jsontext.Value) { got = append(got, method) })
	if err := c.Call(ctx(t), "exec.start", map[string]any{"argv": []string{"true"}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "exec.output" || got[1] != "exec.exit" {
		t.Fatalf("notifications = %v", got)
	}
}

func TestConcurrentCallsGetTheirOwnAnswers(t *testing.T) {
	c := newPair(t, func(r fakeRequest) []any {
		return []any{map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": r.Params}}
	}, nil)
	errs := make(chan error, 20)
	for i := range 20 {
		go func() {
			var got int
			err := c.Call(ctx(t), "echo", i, &got)
			if err == nil && got != i {
				err = errors.New("answer for another call")
			}
			errs <- err
		}()
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepeatedAnswersDoNotStallTheConnection(t *testing.T) {
	c := newPair(t, func(r fakeRequest) []any {
		answer := map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": map[string]any{}}
		// Repeat the answer so some copies arrive while the call has its
		// reply but has not yet dropped its pending entry.
		return slices.Repeat([]any{answer}, 50)
	}, nil)
	for range 20 {
		if err := c.Call(ctx(t), "guest.info", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClosedConnectionFailsPendingAndLaterCalls(t *testing.T) {
	host, guest := net.Pipe()
	c := New(host, nil)
	go func() {
		proto.ReadFrame(guest) // swallow the request, then hang up
		guest.Close()
	}()
	if err := c.Call(ctx(t), "guest.info", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call err = %v, want ErrClosed", err)
	}
	<-c.Done()
	if err := c.Call(ctx(t), "guest.info", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("later call err = %v, want ErrClosed", err)
	}
}

func TestCallStopsWhenContextEnds(t *testing.T) {
	c := newPair(t, func(fakeRequest) []any { return nil }, nil)
	cctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(cctx, "guest.stats", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}
