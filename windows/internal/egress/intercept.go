package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TLS is what the proxy terminates and makes TLS with, for the hosts a
// granted secret is bound to, shared by every VM's proxy: the egress CA,
// a host certificate per name signed by it, and the roots upstream
// certificates are verified with.
type TLS struct {
	CA *CA
	// Roots verifies upstream certificates; nil is the system's.
	Roots *x509.CertPool
	// Now is the clock host certificates are made by; nil is time.Now.
	Now func() time.Time

	mu     sync.Mutex
	leaves map[string]leaf
}

type leaf struct {
	cert tls.Certificate
	made time.Time
}

func (t *TLS) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// maxLeaves bounds the certificates kept.
const maxLeaves = 256

// leaf is host's certificate, made again once it is a day old.
func (t *TLS) leaf(host string) (tls.Certificate, error) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if l, ok := t.leaves[host]; ok && now.Sub(l.made) < leafRenewal {
		return l.cert, nil
	}
	cert, err := t.CA.Leaf(host, now)
	if err != nil {
		return tls.Certificate{}, err
	}
	// A wildcard binding lets a guest name endless hosts: start over
	// rather than keep a certificate for each.
	if t.leaves == nil || len(t.leaves) >= maxLeaves {
		t.leaves = map[string]leaf{}
	}
	t.leaves[host] = leaf{cert, now}
	return cert, nil
}

// intercepts reports whether a CONNECT to host:port is intercepted: a DNS
// name a secret granted to the VM is bound to. An IP literal never is,
// since its certificate could not be checked by name upstream.
func intercepts(host string, port int, granted []string, defined []Secret) bool {
	if _, isIP := parseAddr(host); isIP {
		return false
	}
	return slices.ContainsFunc(defined, func(s Secret) bool {
		return slices.Contains(granted, s.Name) && NewPolicy(s.Hosts).Allows(host, port)
	})
}

// counted counts the bytes read from and written to a connection.
type counted struct {
	net.Conn
	in, out atomic.Int64
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.out.Add(int64(n))
	return n, err
}

func (c *counted) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.in.Add(int64(n))
	return n, err
}

func (c *counted) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// intercept serves a CONNECT to a host a granted secret is bound to:
// answers 200, terminates the guest's TLS with a certificate from the
// egress CA, substitutes the placeholders of the one request head, and
// sends the request over TLS verified against the host's roots, connecting
// to the first of addrs that answers only once the values are in hand: a
// value can take a while, and a provider closes a connection that sends
// nothing meanwhile.
func (p *Proxy) intercept(raw net.Conn, req request, addrs []netip.Addr, settings Settings, defined []Secret, endpoints []TokenEndpoint) {
	c := &counted{Conn: raw}
	r := NewRecord(p.VM, Allow, req.host, req.port, "")
	defer func() {
		r.BytesIn, r.BytesOut = c.in.Load(), c.out.Load()
		p.Log(r)
	}()
	fail := func(reason string) { r.Decision, r.Reason = Fail, reason }
	cert, err := p.TLS.leaf(req.host)
	if err != nil {
		fail("no certificate for the host: " + err.Error())
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		fail("guest closed")
		return
	}
	guest := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	defer guest.Close()
	guest.SetDeadline(time.Now().Add(headTimeout))
	if err := guest.Handshake(); err != nil {
		if strings.Contains(err.Error(), "unknown certificate authority") || strings.Contains(err.Error(), "bad certificate") {
			fail("guest TLS: the guest does not trust the egress CA; recreate a VM made before the CA")
		} else {
			fail("guest TLS: " + err.Error())
		}
		return
	}
	refuse := func(f *Refusal) {
		r.Status, r.Reason = f.Status, f.Reason
		r.Decision = Deny
		if f.Status >= 500 {
			r.Decision = Fail
		}
		body := "glov egress proxy: " + target(req.host, req.port) + ": " + f.Message + "\n"
		io.WriteString(guest, "HTTP/1.1 "+statusLine(f.Status)+"\r\nContent-Type: text/plain\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
		guest.CloseWrite()
	}
	head, rest, ok := readHead(guest)
	if !ok {
		refuse(&Refusal{Status: 400, Message: "no request head within " + headTimeout.String() + " and " + strconv.Itoa(maxHead) + " bytes", Reason: "malformed request"})
		return
	}
	guest.SetDeadline(time.Time{})
	plan, f := NewPlan(head, req.host, req.port, p.VM, settings.Secrets, defined, p.where())
	if f != nil {
		refuse(f)
		return
	}
	r.Method, r.Path = plan.Method, plan.Path
	if len(plan.Secrets) > 0 {
		r.Secrets = plan.Secrets
	}
	if i := slices.IndexFunc(endpoints, func(e TokenEndpoint) bool { return e.Path == plan.Path }); i >= 0 && plan.Method == "POST" {
		p.exchangeToken(guest, req, addrs, settings, defined, plan, endpoints[i], rest, &r, refuse)
		return
	}
	// Only a request that signs in is held to the VM's repositories: an
	// anonymous one (a public clone) reaches what anyone on the internet
	// can.
	if check, ok := forgeOf(req.host, req.port, settings, defined); ok && len(plan.Secrets) > 0 {
		var body []byte
		if body, rest, f = p.checkRepos(guest, req, plan, check, rest); f != nil {
			refuse(f)
			return
		}
		rest = append(body, rest...)
	}
	values := map[string]string{}
	for _, name := range plan.Secrets {
		v, err := p.Value(name)
		if err != nil {
			refuse(&Refusal{Status: 502, Message: err.Error(), Reason: "secret " + name + " unavailable"})
			return
		}
		values[name] = v
	}
	out, f := plan.Apply(values)
	if f != nil {
		refuse(f)
		return
	}
	up, f := p.upstream(req, addrs, settings)
	if f != nil {
		refuse(f)
		return
	}
	defer up.Close()
	if _, err := up.Write(append(out, rest...)); err != nil {
		refuse(&Refusal{Status: 502, Message: "upstream: " + err.Error(), Reason: "upstream unusable"})
		return
	}
	status := &statusReader{r: bufio.NewReader(up)}
	_, _, reset := bridge2(guest, up, status)
	r.Status = status.status
	if reset {
		r.Reason = "upstream reset"
	}
}

// upstream is a TLS connection to the first of addrs that answers,
// verified against the host's roots and the VM's trusted certificates.
func (p *Proxy) upstream(req request, addrs []netip.Addr, settings Settings) (*tls.Conn, *Refusal) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout*time.Duration(len(addrs)))
	defer cancel()
	var conn net.Conn
	var err error
	for _, a := range addrs {
		if conn, err = p.dial(ctx, netip.AddrPortFrom(a, uint16(req.port))); err == nil {
			break
		}
	}
	if conn == nil {
		return nil, &Refusal{Status: 502, Message: "unreachable", Reason: "unreachable"}
	}
	up := tls.Client(conn, &tls.Config{ServerName: req.host, RootCAs: p.TLS.roots(settings.Trust), NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	up.SetDeadline(time.Now().Add(headTimeout))
	if err := up.Handshake(); err != nil {
		up.Close()
		// An upstream certificate the host does not trust fails here, as it
		// would for a direct connection.
		return nil, &Refusal{Status: 502, Message: "upstream TLS: " + err.Error(), Reason: "upstream TLS: " + err.Error()}
	}
	up.SetDeadline(time.Time{})
	return up, nil
}

// statusReader records the HTTP status of the first response line read
// through it.
type statusReader struct {
	r      *bufio.Reader
	seen   bool
	status int
}

func (s *statusReader) Read(p []byte) (int, error) {
	if !s.seen {
		s.seen = true
		if line, err := s.r.Peek(12); err == nil || len(line) >= 12 {
			if f := strings.Fields(string(line)); len(f) >= 2 {
				s.status, _ = strconv.Atoi(f[1])
			}
		}
	}
	return s.r.Read(p)
}

// bridge2 is bridge with the upstream read through from.
func bridge2(guest, up net.Conn, from io.Reader) (out, in int64, upstreamReset bool) {
	fromUp := &resetReader{r: from}
	var wg sync.WaitGroup
	wg.Go(func() {
		in, _ = io.Copy(guest, fromUp)
		if cw, ok := guest.(closeWriter); ok {
			cw.CloseWrite()
		}
	})
	out, _ = io.Copy(up, &resetReader{r: guest})
	if cw, ok := up.(closeWriter); ok {
		cw.CloseWrite()
	}
	wg.Wait()
	return out, in, fromUp.reset
}

func statusLine(status int) string {
	reasons := map[int]string{400: "Bad Request", 403: "Forbidden", 408: "Request Timeout", 502: "Bad Gateway"}
	return strconv.Itoa(status) + " " + reasons[status]
}

// roots verifies an upstream of a VM given trust (glov vm trust): Roots,
// or the system's, plus those certificates.
func (t *TLS) roots(trust []Trust) *x509.CertPool {
	if len(trust) == 0 {
		return t.Roots
	}
	pool := t.Roots
	if pool == nil {
		var err error
		if pool, err = x509.SystemCertPool(); err != nil {
			pool = x509.NewCertPool()
		}
	}
	pool = pool.Clone()
	for _, c := range trust {
		pool.AppendCertsFromPEM([]byte(c.Certificate))
	}
	return pool
}
