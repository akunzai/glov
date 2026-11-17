package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pair is two ends of a loopback TCP connection, which half-close as an
// hv_sock connection does.
func pair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// harness is a proxy whose upstream is the far end of a connection pair,
// and what it did.
type harness struct {
	t         *testing.T
	mu        sync.Mutex
	allow     []string
	private   []string
	openMode  bool
	addrs     []netip.Addr
	lines     []string
	resolved  []string
	connected []string
	upstream  chan net.Conn
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, allow: []string{"api.example.com", "*.ubuntu.com"},
		addrs: []netip.Addr{netip.MustParseAddr("93.184.215.14")}, upstream: make(chan net.Conn, 1)}
}

func (h *harness) proxy() *Proxy {
	return &Proxy{
		VM: "box",
		Settings: func() (Settings, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return Settings{Allow: slices.Clone(h.allow), PrivateAllow: slices.Clone(h.private), Open: h.openMode}, nil
		},
		Resolve: func(_ context.Context, host string) []netip.Addr {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.resolved = append(h.resolved, host)
			return h.addrs
		},
		Local: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("192.0.2.10")} },
		Dial: func(_ context.Context, to netip.AddrPort) (net.Conn, error) {
			h.mu.Lock()
			h.connected = append(h.connected, to.String())
			h.mu.Unlock()
			near, far := pair(h.t)
			h.upstream <- far
			return near, nil
		},
		Log: func(r Record) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.lines = append(h.lines, "egress "+r.Summary())
		},
	}
}

// open serves one guest connection; it returns the guest's end and a
// channel closed when serving ends.
func (h *harness) open() (net.Conn, chan struct{}) {
	guest, host := pair(h.t)
	done := make(chan struct{})
	p := h.proxy()
	go func() { p.ServeConn(host); close(done) }()
	return guest, done
}

func (h *harness) waitUpstream() net.Conn {
	select {
	case c := <-h.upstream:
		return c
	case <-time.After(2 * time.Second):
		h.t.Fatal("the proxy never connected upstream")
		return nil
	}
}

func (h *harness) snapshot() (lines, resolved, connected []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.lines), slices.Clone(h.resolved), slices.Clone(h.connected)
}

func send(t *testing.T, c net.Conn, s string) {
	t.Helper()
	if _, err := io.WriteString(c, s); err != nil {
		t.Fatal(err)
	}
}

// receive reads n bytes, or with n < 0 until the peer closes.
func receive(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n < 0 {
		b, err := io.ReadAll(c)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
		return string(b)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func closeWrite(c net.Conn) { c.(*net.TCPConn).CloseWrite() }

func TestAnAllowedConnectTunnelsBytesAndLogsTheirCount(t *testing.T) {
	h := newHarness(t)
	guest, done := h.open()
	send(t, guest, "CONNECT API.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\nUser-Agent: curl\r\n\r\n")
	if got := receive(t, guest, 39); got != "HTTP/1.1 200 Connection Established\r\n\r\n" {
		t.Fatalf("reply %q", got)
	}
	up := h.waitUpstream()
	send(t, guest, "hello")
	if got := receive(t, up, 5); got != "hello" {
		t.Errorf("upstream got %q", got)
	}
	send(t, up, "world!")
	if got := receive(t, guest, 6); got != "world!" {
		t.Errorf("guest got %q", got)
	}
	up.Close()
	if got := receive(t, guest, -1); got != "" {
		t.Errorf("after close %q", got)
	}
	closeWrite(guest)
	<-done
	lines, _, connected := h.snapshot()
	if !slices.Equal(connected, []string{"93.184.215.14:443"}) {
		t.Errorf("connected %q", connected)
	}
	if !slices.Equal(lines, []string{"egress allow api.example.com:443 in=6 out=5"}) {
		t.Errorf("lines %q", lines)
	}
}

func TestADeniedHostGetsA403AndNothingGoesUpstreamNotEvenDNS(t *testing.T) {
	h := newHarness(t)
	for _, req := range []string{"CONNECT evil.example.net:443 HTTP/1.1\r\n\r\n", "CONNECT api.example.com:22 HTTP/1.1\r\n\r\n", "CONNECT 10.0.0.1:443 HTTP/1.1\r\n\r\n"} {
		guest, done := h.open()
		send(t, guest, req)
		if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 403 Forbidden\r\n") {
			t.Errorf("%q: %q", req, got)
		}
		<-done
	}
	lines, resolved, connected := h.snapshot()
	if len(resolved) != 0 || len(connected) != 0 {
		t.Errorf("resolved %q, connected %q", resolved, connected)
	}
	want := []string{
		"egress deny evil.example.net:443 in=0 out=0 (not in the allowlist)",
		"egress deny api.example.com:22 in=0 out=0 (not in the allowlist)",
		"egress deny 10.0.0.1:443 in=0 out=0 (not in the allowlist)",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines %q", lines)
	}
}

func TestAnAllowedNameThatResolvesToAPrivateOrHostAddressIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, addrs := range [][]string{{"10.0.0.5"}, {"192.0.2.10"}, {"169.254.169.254", "fe80::1"}} {
		h.mu.Lock()
		h.addrs = nil
		for _, a := range addrs {
			h.addrs = append(h.addrs, netip.MustParseAddr(a))
		}
		h.mu.Unlock()
		guest, done := h.open()
		send(t, guest, "CONNECT archive.ubuntu.com:443 HTTP/1.1\r\n\r\n")
		if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 403 Forbidden\r\n") {
			t.Errorf("%q: %q", addrs, got)
		}
		<-done
	}
	lines, _, connected := h.snapshot()
	if len(connected) != 0 {
		t.Errorf("connected %q", connected)
	}
	for _, l := range lines {
		if l != "egress deny archive.ubuntu.com:443 in=0 out=0 (no public address)" {
			t.Errorf("line %q", l)
		}
	}
}

func TestRefusalsNameWhatTheModeAndAPrivateEntryAllow(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	h.private = []string{"api.example.com"}
	h.addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	h.mu.Unlock()
	for _, target := range []string{"api.example.com:443", "a.ubuntu.com:443", "a..example.com:443"} {
		if target == "a..example.com:443" {
			h.mu.Lock()
			h.openMode = true
			h.mu.Unlock()
		}
		guest, done := h.open()
		send(t, guest, "CONNECT "+target+" HTTP/1.1\r\n\r\n")
		receive(t, guest, -1)
		<-done
	}
	lines, _, _ := h.snapshot()
	want := []string{
		"egress deny api.example.com:443 in=0 out=0 (no public or private-network address)",
		"egress deny a.ubuntu.com:443 in=0 out=0 (no public address)",
		"egress deny a..example.com:443 in=0 out=0 (not a host and port)",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines %q", lines)
	}
}

func TestAPlainHTTPRequestGoesUpstreamInOriginFormWithoutProxyHeaders(t *testing.T) {
	h := newHarness(t)
	guest, done := h.open()
	send(t, guest, "GET http://archive.ubuntu.com/ubuntu/dists/x?y=1 HTTP/1.1\r\nHost: archive.ubuntu.com\r\n"+
		"Proxy-Connection: keep-alive\r\nProxy-Authorization: Basic c2VjcmV0\r\nConnection: keep-alive\r\nUser-Agent: apt\r\n\r\nBODY")
	up := h.waitUpstream()
	want := "GET /ubuntu/dists/x?y=1 HTTP/1.1\r\nHost: archive.ubuntu.com\r\nUser-Agent: apt\r\nConnection: close\r\n\r\nBODY"
	if got := receive(t, up, len(want)); got != want {
		t.Errorf("upstream got %q", got)
	}
	send(t, up, "HTTP/1.1 200 OK\r\n\r\n")
	up.Close()
	if got := receive(t, guest, -1); got != "HTTP/1.1 200 OK\r\n\r\n" {
		t.Errorf("guest got %q", got)
	}
	closeWrite(guest)
	<-done
	lines, _, connected := h.snapshot()
	if !slices.Equal(connected, []string{"93.184.215.14:80"}) {
		t.Errorf("connected %q", connected)
	}
	if !slices.Equal(lines, []string{"egress allow archive.ubuntu.com:80 in=19 out=" + strconv.Itoa(len(want))}) {
		t.Errorf("lines %q", lines)
	}
}

// The host reached resets the connection, which is upstream's doing and
// not the proxy's.
func TestAnUpstreamResetIsNamedInTheLog(t *testing.T) {
	h := newHarness(t)
	guest, done := h.open()
	send(t, guest, "GET http://archive.ubuntu.com/ubuntu/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n")
	up := h.waitUpstream()
	want := "GET /ubuntu/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\nConnection: close\r\n\r\n"
	if got := receive(t, up, len(want)); got != want {
		t.Errorf("upstream got %q", got)
	}
	up.(*net.TCPConn).SetLinger(0) // Close sends RST
	up.Close()
	if got := receive(t, guest, -1); got != "" {
		t.Errorf("guest got %q", got)
	}
	closeWrite(guest)
	<-done
	lines, _, _ := h.snapshot()
	if !slices.Equal(lines, []string{"egress allow archive.ubuntu.com:80 in=0 out=" + strconv.Itoa(len(want)) + " (upstream reset)"}) {
		t.Errorf("lines %q", lines)
	}
}

func TestAPlainHTTPRequestWhoseHostHeaderNamesAnotherSiteIsRefused(t *testing.T) {
	h := newHarness(t)
	for _, req := range []string{
		"GET http://archive.ubuntu.com/ HTTP/1.1\r\nHost: evil.example.net\r\n\r\n",
		"GET http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\nHost: evil.example.net\r\n\r\n",
		"GET http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n Proxy-Authorization: x\r\n\r\n",
	} {
		guest, done := h.open()
		send(t, guest, req)
		if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 400 Bad Request\r\n") {
			t.Errorf("%q: %q", req, got)
		}
		<-done
	}
	if _, _, connected := h.snapshot(); len(connected) != 0 {
		t.Errorf("connected %q", connected)
	}
	guest, done := h.open()
	send(t, guest, "GET http://archive.ubuntu.com:80/ HTTP/1.1\r\nhost:  Archive.Ubuntu.com:80 \r\n\r\n")
	h.waitUpstream().Close()
	receive(t, guest, -1)
	closeWrite(guest)
	<-done
}

func TestAMalformedRequestGetsA400WithoutEchoingIt(t *testing.T) {
	h := newHarness(t)
	reqs := []string{"HELLO\r\n\r\n", "GET relative HTTP/1.1\r\nHost: api.example.com\r\n\r\n", "CONNECT api.example.com HTTP/1.1\r\n\r\n",
		"CONNECT bad\x1bhost:443 HTTP/1.1\r\n\r\n", "CONNECT [fe80::1%eth0]:443 HTTP/1.1\r\n\r\n"}
	for _, req := range reqs {
		guest, done := h.open()
		send(t, guest, req)
		got := receive(t, guest, -1)
		if !strings.HasPrefix(got, "HTTP/1.1 400 Bad Request\r\n") || strings.Contains(got, "api.example.com") {
			t.Errorf("%q: %q", req, got)
		}
		<-done
	}
	lines, _, connected := h.snapshot()
	if len(connected) != 0 {
		t.Errorf("connected %q", connected)
	}
	if !slices.Equal(lines, slices.Repeat([]string{"egress deny - in=0 out=0 (malformed request)"}, len(reqs))) {
		t.Errorf("lines %q", lines)
	}
}

func TestAnOversizedHeadIsMalformed(t *testing.T) {
	h := newHarness(t)
	guest, done := h.open()
	go io.WriteString(guest, "CONNECT api.example.com:443 HTTP/1.1\r\nX: "+strings.Repeat("a", maxHead)+"\r\n\r\n")
	if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 400 Bad Request\r\n") {
		t.Errorf("reply %q", got)
	}
	<-done
}

func TestAnAllowlistChangeAppliesToTheNextConnection(t *testing.T) {
	h := newHarness(t)
	h.allow = nil
	guest, done := h.open()
	send(t, guest, "CONNECT api.example.com:443 HTTP/1.1\r\n\r\n")
	if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 403") {
		t.Errorf("before %q", got)
	}
	<-done
	h.mu.Lock()
	h.allow = []string{"api.example.com"}
	h.mu.Unlock()
	guest, done = h.open()
	send(t, guest, "CONNECT api.example.com:443 HTTP/1.1\r\n\r\n")
	if got := receive(t, guest, 39); !strings.HasPrefix(got, "HTTP/1.1 200") {
		t.Errorf("after %q", got)
	}
	h.waitUpstream().Close()
	closeWrite(guest)
	<-done
}

func TestAnUnreachableAllowedHostGetsA502(t *testing.T) {
	var lines []string
	p := &Proxy{VM: "box", Settings: func() (Settings, error) { return Settings{Allow: []string{"api.example.com"}}, nil },
		Resolve: func(context.Context, string) []netip.Addr { return []netip.Addr{netip.MustParseAddr("93.184.215.14")} },
		Local:   func() []netip.Addr { return nil },
		Dial:    func(context.Context, netip.AddrPort) (net.Conn, error) { return nil, errors.New("refused") },
		Log:     func(r Record) { lines = append(lines, "egress "+r.Summary()) }}
	guest, host := pair(t)
	send(t, guest, "CONNECT api.example.com:443 HTTP/1.1\r\n\r\n")
	p.ServeConn(host)
	if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 502 Bad Gateway\r\n") {
		t.Errorf("reply %q", got)
	}
	if !slices.Equal(lines, []string{"egress fail api.example.com:443 in=0 out=0 (unreachable)"}) {
		t.Errorf("lines %q", lines)
	}
}

func TestAnAllowlistThatCannotBeReadDeniesEverything(t *testing.T) {
	var lines []string
	p := &Proxy{VM: "box", Settings: func() (Settings, error) { return Settings{}, errors.New("gone") },
		Log: func(r Record) { lines = append(lines, r.Summary()) }}
	guest, host := pair(t)
	send(t, guest, "CONNECT api.example.com:443 HTTP/1.1\r\n\r\n")
	p.ServeConn(host)
	if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 403") {
		t.Errorf("reply %q", got)
	}
}
