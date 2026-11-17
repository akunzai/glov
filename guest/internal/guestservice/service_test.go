//go:build !windows

package guestservice_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akunzai/glov/guest/internal/guestservice"
	"github.com/akunzai/glov/guest/internal/mirror"
	"github.com/akunzai/glov/guest/proto"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

type msg struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  *proto.Error   `json:"error"`
}

// host plays the host side of the channel against a running Guest Service.
type host struct {
	t     *testing.T
	conns chan net.Conn
	c     net.Conn
	next  int
}

func start(t *testing.T) *host {
	t.Helper()
	h := &host{t: t, conns: make(chan net.Conn)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	a := guestservice.New("test")
	go func() {
		a.Run(ctx, func() (io.ReadWriteCloser, error) {
			select {
			case c := <-h.conns:
				return c, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, 10*time.Millisecond)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		if h.c != nil {
			h.c.Close()
		}
		<-done
	})
	return h
}

// connect attaches a fresh stream and returns the guest.ready notification.
func (h *host) connect() msg {
	h.t.Helper()
	a, b := net.Pipe()
	h.conns <- b
	h.c = a
	m := h.recv()
	if m.Method != "guest.ready" {
		h.t.Fatalf("first message = %s, want guest.ready", m.Method)
	}
	return m
}

func (h *host) recv() msg {
	h.t.Helper()
	h.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, err := proto.ReadFrame(h.c)
	if err != nil {
		h.t.Fatal(err)
	}
	var m msg
	if err := json.Unmarshal(body, &m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

// call sends a request and returns its response, plus any notifications that
// arrived before it.
func (h *host) call(method, params string) (resp msg, before []msg) {
	h.t.Helper()
	h.next++
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, h.next, method, params)
	if err := proto.WriteFrame(h.c, []byte(req)); err != nil {
		h.t.Fatal(err)
	}
	for {
		m := h.recv()
		if m.Method == "" {
			return m, before
		}
		before = append(before, m)
	}
}

func (h *host) mustCall(method, params string) msg {
	h.t.Helper()
	r, _ := h.call(method, params)
	if r.Error != nil {
		h.t.Fatalf("%s: %v", method, r.Error)
	}
	return r
}

func TestReadyAndInfo(t *testing.T) {
	h := start(t)
	ready := h.connect()
	var info guestservice.Info
	if err := json.Unmarshal(ready.Params, &info); err != nil {
		t.Fatal(err)
	}
	if info.ProtocolVersion != 1 || info.Version != "test" || info.OS == "" || info.Arch == "" || info.BootID == "" {
		t.Fatalf("guest.ready = %+v", info)
	}
	var got guestservice.Info
	json.Unmarshal(h.mustCall("guest.info", `{}`).Result, &got)
	if got.BootID != info.BootID || got.Hostname != info.Hostname {
		t.Fatalf("guest.info %+v differs from guest.ready %+v", got, info)
	}
}

func TestExecOverTheWire(t *testing.T) {
	h := start(t)
	h.connect()
	resp, before := h.call("exec.start", `{"argv":["/bin/sh","-c","echo hi; echo oops >&2; exit 7"]}`)
	if len(before) != 0 {
		t.Fatalf("notifications %v overtook the exec.start response", before)
	}
	var started struct {
		Pid int `json:"pid"`
	}
	json.Unmarshal(resp.Result, &started)
	if started.Pid == 0 {
		t.Fatalf("result = %s", resp.Result)
	}

	var out, errOut strings.Builder
	for {
		m := h.recv()
		var p struct {
			Pid    int    `json:"pid"`
			Stream string `json:"stream"`
			Data   []byte `json:"data"`
			Code   int    `json:"code"`
		}
		json.Unmarshal(m.Params, &p)
		if p.Pid != started.Pid {
			t.Fatalf("notification for pid %d, want %d", p.Pid, started.Pid)
		}
		if m.Method == "exec.output" {
			if p.Stream == "stdout" {
				out.Write(p.Data)
			} else {
				errOut.Write(p.Data)
			}
			continue
		}
		if m.Method != "exec.exit" || p.Code != 7 {
			t.Fatalf("got %s %s", m.Method, m.Params)
		}
		break
	}
	if out.String() != "hi\n" || errOut.String() != "oops\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

// power.shutdown is gone everywhere and power.reboot is Windows-only: the host
// stops guests with ACPI and reboots a Linux guest by stopping and starting it.
func TestPowerMethodsAreNotServedOffWindows(t *testing.T) {
	h := start(t)
	h.connect()
	for _, method := range []string{"power.shutdown", "power.reboot"} {
		if r, _ := h.call(method, `{}`); r.Error == nil || r.Error.Code != proto.CodeNoMethod {
			t.Errorf("%s: error = %+v, want method not found", method, r.Error)
		}
	}
}

func TestFilesOverTheWire(t *testing.T) {
	h := start(t)
	h.connect()
	path := t.TempDir() + "/f.txt"
	// "aGk=" is base64 for "hi", as the protocol's data field requires.
	h.mustCall("file.write", fmt.Sprintf(`{"path":%q,"offset":0,"data":"aGk=","truncate":true}`, path))
	r := h.mustCall("file.read", fmt.Sprintf(`{"path":%q,"offset":0,"length":100}`, path))
	if string(r.Result) != `{"data":"aGk=","eof":true}` {
		t.Fatalf("file.read = %s", r.Result)
	}
	if r, _ := h.call("file.read", fmt.Sprintf(`{"path":%q,"offset":0,"length":524289}`, path)); r.Error == nil || r.Error.Code != proto.CodeInvalidParams {
		t.Fatalf("over-long read: %+v", r.Error)
	}
	if r, _ := h.call("file.stat", `{"path":"/nonexistent/x"}`); r.Error != nil || !strings.Contains(string(r.Result), `"exists":false`) {
		t.Fatalf("stat missing: %+v %s", r.Error, r.Result)
	}
}

func TestReconnectSendsReadyAgainAndFlushesPendingNotifications(t *testing.T) {
	h := start(t)
	h.connect()
	h.mustCall("exec.start", `{"argv":["/bin/sh","-c","sleep 0.3; echo late"]}`)

	h.c.Close() // the host goes away while the process is still running
	time.Sleep(500 * time.Millisecond)
	h.connect() // a new session: guest.ready first, then what was held back

	var sawLate, sawExit bool
	for !sawExit {
		m := h.recv()
		switch m.Method {
		case "exec.output":
			sawLate = sawLate || strings.Contains(string(m.Params), "bGF0ZQo=") // "late\n"
		case "exec.exit":
			sawExit = true
		}
	}
	if !sawLate {
		t.Fatal("output produced while disconnected was lost")
	}
}

// chanListener is a transport.Listener fed by the test.
type chanListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *chanListener) Accept() (io.ReadWriteCloser, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

// A new host connection replaces the current one and gets guest.ready; the
// old one is closed, since after a snapshot restore the guest never sees it end.
func TestListenerNewConnectionReplacesTheOldOne(t *testing.T) {
	l := newChanListener()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { guestservice.New("test").RunListener(ctx, l, 10*time.Millisecond); close(done) }()
	defer func() { cancel(); <-done }()

	h := &host{t: t}
	connect := func() net.Conn {
		a, b := net.Pipe()
		l.conns <- b
		h.c = a
		if m := h.recv(); m.Method != "guest.ready" {
			t.Fatalf("first message = %s, want guest.ready", m.Method)
		}
		return a
	}
	first := connect()
	h.mustCall("guest.info", `{}`)
	second := connect()

	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := proto.ReadFrame(first); err == nil {
		t.Fatal("the replaced connection is still open")
	}
	h.c = second
	h.mustCall("guest.info", `{}`)

	h.c.Close() // the host drops; the Guest Service goes back to accepting
	connect()
	h.mustCall("guest.info", `{}`)
}

func TestListenerReturnsWhenContextEnds(t *testing.T) {
	l := newChanListener()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { guestservice.New("test").RunListener(ctx, l, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunListener did not return")
	}
}

func TestShareSyncOverTheWire(t *testing.T) {
	state := t.TempDir()
	old := mirror.StateDir
	mirror.StateDir = state
	t.Cleanup(func() { mirror.StateDir = old })
	h := start(t)
	h.connect()
	root, err := filepath.EvalSymlinks(t.TempDir()) // the share root may not lie below a link
	if err != nil {
		t.Fatal(err)
	}
	// "aGk=" is base64 for "hi"; its SHA-256 follows.
	const hiHash = "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4"
	r := h.mustCall("sync.apply", fmt.Sprintf(`{"id":"proj","root":%q,"ops":[{"op":"write","path":"a/hi.txt","offset":0,"data":"aGk=","end":true,"hash":%q}],"commit":true}`, root, hiHash))
	if string(r.Result) != `{"failures":[]}` {
		t.Fatalf("sync.apply = %s", r.Result)
	}
	r = h.mustCall("sync.manifest", fmt.Sprintf(`{"id":"proj","root":%q}`, root))
	want := fmt.Sprintf(`{"entries":[{"path":"a","dir":true,"size":0,"hash":""},{"path":"a/hi.txt","size":2,"mtime":N,"hash":%q}],"total":2,"hashed":0,"fresh":false}`, hiHash)
	if regexp.MustCompile(`"mtime":[1-9][0-9]*`).ReplaceAllString(string(r.Result), `"mtime":N`) != want {
		t.Fatalf("sync.manifest = %s\nwant %s", r.Result, want)
	}
	if _, err := os.Stat(state + "/proj.json"); err != nil {
		t.Fatal(err)
	}
}
