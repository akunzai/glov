package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream is an HTTPS server for api.example.com with a CA of its own,
// answering each request with "ok" and keeping its head.
type upstream struct {
	ca     *CA
	l      net.Listener
	mu     sync.Mutex
	heads  []string
	bodies []string
	// respond answers a request; nil answers 200 "ok".
	respond func(head, body string) string
}

func newUpstream(t *testing.T) *upstream {
	ca, err := LoadOrCreateCA(filepath.Join(t.TempDir(), CADir))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.Leaf("api.example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	u := &upstream{ca: ca, l: l}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				head, rest, ok := readHead(c)
				if !ok {
					return
				}
				body := rest
				if _, v, ok := strings.Cut(strings.ToLower(string(head)), "\r\ncontent-length: "); ok {
					n, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(v, "\r\n", 2)[0]))
					if n > len(rest) {
						more := make([]byte, n-len(rest))
						io.ReadFull(c, more)
						body = append(rest, more...)
					}
				}
				u.mu.Lock()
				u.heads = append(u.heads, string(head))
				u.bodies = append(u.bodies, string(body))
				respond := u.respond
				u.mu.Unlock()
				if respond != nil {
					io.WriteString(c, respond(string(head), string(body)))
					return
				}
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}()
		}
	}()
	return u
}

func (u *upstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.heads...)
}

func pool(ca *CA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(ca.PEM)
	return p
}

// tlsProxy is a proxy for VM box, granted gh, whose upstream is u (or
// nothing, with u nil), and what it logged and did in order.
type tlsProxy struct {
	*Proxy
	mu     sync.Mutex
	lines  []string
	events []string
}

func newTLSProxy(t *testing.T, u *upstream, egressCA *CA) *tlsProxy {
	tp := &tlsProxy{}
	roots := x509.NewCertPool()
	if u != nil {
		roots = pool(u.ca)
	}
	tp.Proxy = &Proxy{
		VM: "box",
		Settings: func() (Settings, error) {
			return Settings{Allow: []string{"api.example.com", "other.example.com"}, Secrets: []string{"gh"}}, nil
		},
		Resolve: func(context.Context, string) []netip.Addr { return []netip.Addr{netip.MustParseAddr("93.184.215.14")} },
		Local:   func() []netip.Addr { return nil },
		Dial: func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
			tp.event("connect")
			if u == nil {
				return nil, errors.New("refused")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", u.l.Addr().String())
		},
		Log: func(r Record) { tp.mu.Lock(); tp.lines = append(tp.lines, "egress "+r.Summary()); tp.mu.Unlock() },
		TLS: &TLS{CA: egressCA, Roots: roots},
		Secrets: func() ([]Secret, error) {
			return []Secret{{Name: "gh", Hosts: []string{"api.example.com"}}, {Name: "npm", Hosts: []string{"registry.example.org"}}}, nil
		},
		Value: func(string) (string, error) {
			time.Sleep(100 * time.Millisecond)
			tp.event("value")
			return "tok-REAL", nil
		},
	}
	return tp
}

func (tp *tlsProxy) event(e string) { tp.mu.Lock(); tp.events = append(tp.events, e); tp.mu.Unlock() }

func (tp *tlsProxy) snapshot() (lines, events []string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]string(nil), tp.lines...), append([]string(nil), tp.events...)
}

// exchange sends a CONNECT, then request over TLS trusting trust, and
// returns the plaintext answer; it waits for the proxy to finish.
func (tp *tlsProxy) exchange(t *testing.T, request string, trust *CA) (string, error) {
	t.Helper()
	guest, host := pair(t)
	done := make(chan struct{})
	go func() { tp.ServeConn(host); close(done) }()
	defer func() { <-done }()
	send(t, guest, "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n")
	if got := receive(t, guest, 39); got != "HTTP/1.1 200 Connection Established\r\n\r\n" {
		t.Fatalf("CONNECT answer %q", got)
	}
	c := tls.Client(guest, &tls.Config{ServerName: "api.example.com", RootCAs: pool(trust)})
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.Handshake(); err != nil {
		return "", err
	}
	if _, err := io.WriteString(c, request); err != nil {
		return "", err
	}
	b, err := io.ReadAll(bufio.NewReader(c))
	if err != nil && len(b) == 0 {
		return "", err
	}
	return string(b), nil
}

func egressCA(t *testing.T) *CA {
	ca, err := LoadOrCreateCA(filepath.Join(t.TempDir(), CADir))
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestABoundHostGetsTheValueOverTLSAndTheLogNamesOnlyTheSecret(t *testing.T) {
	u, ca := newUpstream(t), egressCA(t)
	tp := newTLSProxy(t, u, ca)
	ok, err := tp.exchange(t, "GET /user HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n", ca)
	if err != nil || !strings.HasPrefix(ok, "HTTP/1.1 200 OK") || !strings.HasSuffix(ok, "ok") {
		t.Fatalf("answer %q, %v", ok, err)
	}
	if got := u.seen(); len(got) != 1 || got[0] != "GET /user HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer tok-REAL\r\nConnection: close\r\n\r\n" {
		t.Fatalf("upstream saw %q", got)
	}
	denied, err := tp.exchange(t, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:npm\r\n\r\n", ca)
	if err != nil || !strings.HasPrefix(denied, "HTTP/1.1 403 Forbidden") || !strings.Contains(denied, "glov secret grant box npm") {
		t.Fatalf("denied %q, %v", denied, err)
	}
	// A guest that does not trust the egress CA gets no request through.
	if _, err := tp.exchange(t, "GET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n", u.ca); err == nil {
		t.Error("a guest trusting another CA finished its handshake")
	}
	if len(u.seen()) != 1 {
		t.Errorf("upstream saw %d requests", len(u.seen()))
	}
	lines, events := tp.snapshot()
	if len(lines) != 3 ||
		!strings.HasPrefix(lines[0], "egress allow api.example.com:443 GET /user 200 in=") || !strings.HasSuffix(lines[0], " secrets=gh") ||
		!strings.Contains(lines[1], "deny api.example.com:443 403") || !strings.HasSuffix(lines[1], "(secret npm not granted)") ||
		!strings.HasPrefix(lines[2], "egress fail api.example.com:443") || !strings.HasSuffix(lines[2], "(guest TLS: the guest does not trust the egress CA; recreate a VM made before the CA)") {
		t.Errorf("log %q", lines)
	}
	if strings.Contains(strings.Join(lines, ""), "REAL") {
		t.Error("the log holds the value")
	}
	// The values were read before the one connection upstream; the refused
	// request never connected.
	if strings.Join(events, ",") != "value,connect" {
		t.Errorf("events %q", events)
	}
}

func TestAnUpstreamOutOfReachOnceTheValuesAreInIsA502(t *testing.T) {
	ca := egressCA(t)
	tp := newTLSProxy(t, nil, ca)
	got, err := tp.exchange(t, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n", ca)
	if err != nil || !strings.HasPrefix(got, "HTTP/1.1 502 Bad Gateway") {
		t.Fatalf("answer %q, %v", got, err)
	}
	if lines, _ := tp.snapshot(); len(lines) != 1 || !strings.Contains(lines[0], "fail api.example.com:443") || !strings.HasSuffix(lines[0], "(unreachable)") {
		t.Errorf("log %q", lines)
	}
}

func TestAnUpstreamTheHostDoesNotTrustIsNeverSentTheValue(t *testing.T) {
	u, ca := newUpstream(t), egressCA(t)
	tp := newTLSProxy(t, u, ca)
	tp.TLS.Roots = pool(ca)
	got, err := tp.exchange(t, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n", ca)
	if err != nil || !strings.HasPrefix(got, "HTTP/1.1 502 Bad Gateway") || len(u.seen()) != 0 {
		t.Fatalf("answer %q, %v, upstream saw %d", got, err, len(u.seen()))
	}
}

func TestAnUpstreamSignedByACAGivenToTheVMIsTrusted(t *testing.T) {
	u, ca := newUpstream(t), egressCA(t)
	tp := newTLSProxy(t, u, ca)
	tp.TLS.Roots = pool(ca)
	settings := tp.Settings
	tp.Settings = func() (Settings, error) {
		s, err := settings()
		s.Trust = []Trust{{Name: "corp", Certificate: string(u.ca.PEM)}}
		return s, err
	}
	got, err := tp.exchange(t, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n", ca)
	if err != nil || !strings.HasPrefix(got, "HTTP/1.1 200 OK") || len(u.seen()) != 1 {
		t.Fatalf("answer %q, %v, upstream saw %d", got, err, len(u.seen()))
	}
	// The VM's CA joins a copy: the host roots stay as they were.
	if _, err := u.ca.cert.Verify(x509.VerifyOptions{Roots: tp.TLS.Roots}); err == nil {
		t.Error("the VM's CA was added to the shared roots")
	}
}

func TestAPlainHTTPRequestCarryingAPlaceholderIsRefusedBeforeConnecting(t *testing.T) {
	tp := newTLSProxy(t, nil, egressCA(t))
	guest, host := pair(t)
	done := make(chan struct{})
	go func() { tp.ServeConn(host); close(done) }()
	send(t, guest, "GET http://api.example.com/ HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Basic dTpnbG92LXNlY3JldDpnaA==\r\n\r\n")
	if got := receive(t, guest, -1); !strings.HasPrefix(got, "HTTP/1.1 403 Forbidden") {
		t.Fatalf("answer %q", got)
	}
	<-done
	lines, events := tp.snapshot()
	if len(events) != 0 || len(lines) != 1 || lines[0] != "egress deny api.example.com:80 in=0 out=0 (a secret placeholder goes only over HTTPS to a host the secret is bound to)" {
		t.Errorf("log %q, events %q", lines, events)
	}
}

func TestOnlyDNSNamesBoundToAGrantedSecretAreIntercepted(t *testing.T) {
	d := []Secret{{Name: "gh", Hosts: []string{"api.example.com"}}, {Name: "npm", Hosts: []string{"registry.example.org"}}, {Name: "ip", Hosts: []string{"93.184.215.14"}}}
	for _, c := range []struct {
		host    string
		port    int
		granted string
		want    bool
	}{
		{"api.example.com", 443, "gh", true},
		{"api.example.com", 443, "npm", false},
		{"api.example.com", 8443, "gh", false},
		{"other.example.com", 443, "gh", false},
		{"93.184.215.14", 443, "ip", false},
	} {
		if got := intercepts(c.host, c.port, []string{c.granted}, d); got != c.want {
			t.Errorf("intercepts(%s:%d, %s) = %v", c.host, c.port, c.granted, got)
		}
	}
}

func TestTheCAIsMadeOnceAndAHostCertificateLastsAWeek(t *testing.T) {
	dir := filepath.Join(t.TempDir(), CADir)
	a, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCA(dir)
	if err != nil || string(a.PEM) != string(b.PEM) {
		t.Fatalf("a second load made another CA (%v)", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dir)); len(entries) != 1 {
		t.Errorf("left %d entries beside the CA", len(entries))
	}
	now := time.Now()
	cert, err := a.Leaf("api.example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "api.example.com", Roots: pool(a)}); err != nil {
		t.Error(err)
	}
	if d := leaf.NotAfter.Sub(now) - LeafLifetime; d > 0 || d < -time.Second || leaf.IsCA {
		t.Errorf("leaf until %v, CA %v", leaf.NotAfter, leaf.IsCA)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil || strings.Contains(err.Error(), "not a key") {
		t.Errorf("a broken key read as %v", err)
	}
}

// eager sends its CONNECT with the first write, the ClientHello, and skips
// the proxy's 200 before the first read, as a client that does not wait.
type eager struct {
	net.Conn
	sent, answered bool
}

func (e *eager) Write(b []byte) (int, error) {
	if !e.sent {
		e.sent = true
		if _, err := e.Conn.Write(append([]byte("CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"), b...)); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return e.Conn.Write(b)
}

func (e *eager) Read(b []byte) (int, error) {
	if !e.answered {
		e.answered = true
		answer := make([]byte, 39)
		if _, err := io.ReadFull(e.Conn, answer); err != nil || string(answer) != "HTTP/1.1 200 Connection Established\r\n\r\n" {
			return 0, errors.New("CONNECT answer " + string(answer))
		}
	}
	return e.Conn.Read(b)
}

func TestAClientHelloSentWithTheConnectIsKept(t *testing.T) {
	u, ca := newUpstream(t), egressCA(t)
	tp := newTLSProxy(t, u, ca)
	guest, host := pair(t)
	done := make(chan struct{})
	go func() { tp.ServeConn(host); close(done) }()
	defer func() { <-done }()
	c := tls.Client(&eager{Conn: guest}, &tls.Config{ServerName: "api.example.com", RootCAs: pool(ca)})
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(c); !strings.HasPrefix(string(b), "HTTP/1.1 200 OK") {
		t.Errorf("answer %q", b)
	}
}
