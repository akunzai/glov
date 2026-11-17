package egress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// Port is the host's hv_sock (vsock) port the guest connects to.
	Port = 4024
	// MaxConns is how many connections of one VM are served at once.
	MaxConns    = 256
	maxHead     = 16 << 10
	headTimeout = 10 * time.Second
	dialTimeout = 5 * time.Second
)

// Proxy is the host end of one isolated VM's egress proxy: each guest
// connection carries one HTTP proxy request, which Serve allows or denies
// by the VM's allowlist.
type Proxy struct {
	VM string
	// Settings are the VM's egress settings as vm.json has them now, so a
	// change applies to the next connection.
	Settings func() (Settings, error)
	// TLS terminates and makes TLS for the hosts a granted secret is bound
	// to; nil intercepts nothing.
	TLS *TLS
	// Secrets is every secret defined on the host, without values; nil
	// defines none.
	Secrets func() ([]Secret, error)
	// Value is one secret's value. Its error names the secret, never a
	// value.
	Value func(name string) (string, error)
	// Where names where the owner defines and grants secrets, for refusals;
	// "" is "on the Windows host".
	Where string
	// Resolve is the addresses of a host name or literal, resolved on the
	// host; nil is the system resolver.
	Resolve func(ctx context.Context, host string) []netip.Addr
	// Local is the addresses of the host's own interfaces; nil reads them.
	Local func() []netip.Addr
	// Dial connects to an address the proxy checked; nil is TCP within 5 s.
	Dial func(ctx context.Context, to netip.AddrPort) (net.Conn, error)
	// Nodes is the repository (owner/name, lower case) of each GitHub node
	// ID, "" for a node of no repository (a user), asked of host with the
	// VM's git secret token; nil asks GitHub (GitHubNodes).
	Nodes func(ctx context.Context, host, token string, ids []string) (map[string]string, error)
	// TokenEndpoints is the token endpoints of a VM's subscribed harnesses
	// (Settings.Subscriptions); nil has none.
	TokenEndpoints func(subscriptions []string) []TokenEndpoint
	// Keep stores a token a token endpoint answered as secret name, bound
	// to hosts, replacing its value; nil keeps none.
	Keep func(name string, hosts []string, value string) error
	// Log takes one record per connection, when it ends.
	Log func(Record)
}

// nodes is shared by every proxy that has no Nodes: its answers hold for
// any VM.
var githubNodes GitHubNodes

// Serve answers each connection l accepts, at most MaxConns at once; past
// that a connection is closed. It returns when l fails, as when closed.
func (p *Proxy) Serve(l net.Listener) {
	slots := make(chan struct{}, MaxConns)
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); p.ServeConn(c) }()
		default:
			c.Close()
			p.Log(NewRecord(p.VM, Deny, "", 0, "more than "+strconv.Itoa(MaxConns)+" connections"))
		}
	}
}

// request is a parsed proxy request.
type request struct {
	// host is lowercased, without a trailing dot; an IPv6 literal without
	// brackets.
	host string
	port int
	// upstreamHead is, for plain HTTP, the head to send upstream (origin
	// form, no Proxy-* headers, Connection: close).
	upstreamHead []byte
}

// ServeConn serves one guest connection and closes c. It blocks until both
// directions end.
func (p *Proxy) ServeConn(c net.Conn) {
	defer c.Close()
	head, rest, ok := readHead(c)
	var req request
	if ok {
		req, ok = parse(head)
	}
	if !ok {
		reply(c, "400 Bad Request", "")
		p.Log(NewRecord(p.VM, Deny, "", 0, "malformed request"))
		return
	}
	// refuse answers without connecting: deny (403) for what the policy
	// refuses, fail (502) for an allowed target out of reach.
	refuse := func(reason string, failed bool) {
		status, decision := "403 Forbidden", Deny
		if failed {
			status, decision = "502 Bad Gateway", Fail
		}
		reply(c, status, "glov egress proxy: "+target(req.host, req.port)+": "+reason+"\n")
		p.Log(NewRecord(p.VM, decision, req.host, req.port, reason))
	}
	settings, err := p.Settings()
	if err != nil || !settings.Permits(req.host, req.port) {
		reason := "not in the allowlist"
		if err == nil && settings.Open {
			reason = "not a host and port"
		}
		refuse(reason, false)
		return
	}
	if req.upstreamHead != nil && MentionsPlaceholder(append(slices.Clone(req.upstreamHead), rest...)) {
		refuse("a secret placeholder goes only over HTTPS to a host the secret is bound to", false)
		return
	}
	var endpoints []TokenEndpoint
	if p.TokenEndpoints != nil && len(settings.Subscriptions) > 0 {
		endpoints = p.TokenEndpoints(settings.Subscriptions)
	}
	var defined []Secret
	if (len(settings.Secrets) > 0 || p.TLS != nil && req.upstreamHead == nil && len(endpoints) > 0) && p.Secrets != nil {
		if defined, err = p.Secrets(); err != nil {
			refuse("cannot read the secrets: "+err.Error(), true)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), headTimeout)
	defer cancel()
	addrs := p.resolve(ctx, req.host)
	if len(addrs) == 0 {
		refuse("unresolved", true)
		return
	}
	local, private := p.local(), settings.ReachesPrivate(req.host, req.port)
	var reachable []netip.Addr
	for _, a := range addrs {
		if Reachable(a, local, private) {
			reachable = append(reachable, a)
		}
	}
	if len(reachable) == 0 {
		if private {
			refuse("no public or private-network address", false)
		} else {
			refuse("no public address", false)
		}
		return
	}
	tokenHosts := slices.DeleteFunc(slices.Clone(endpoints), func(e TokenEndpoint) bool { return e.Host != req.host || req.port != 443 })
	if p.TLS != nil && req.upstreamHead == nil && (len(tokenHosts) > 0 || intercepts(req.host, req.port, settings.Secrets, defined)) {
		// A client may send its ClientHello before the 200.
		p.intercept(prefixed{c, io.MultiReader(bytes.NewReader(rest), c)}, req, reachable, settings, defined, tokenHosts)
		return
	}
	var up net.Conn
	for _, a := range reachable {
		if up, err = p.dial(ctx, netip.AddrPortFrom(a, uint16(req.port))); err == nil {
			break
		}
	}
	if up == nil {
		refuse("unreachable", true)
		return
	}
	defer up.Close()
	first := rest
	if req.upstreamHead != nil {
		first = append(req.upstreamHead, rest...)
	} else if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if len(first) > 0 {
		if _, err := up.Write(first); err != nil {
			return
		}
	}
	out, in, reset := bridge(c, up)
	reason := ""
	if reset {
		reason = "upstream reset"
	}
	r := NewRecord(p.VM, Allow, req.host, req.port, reason)
	r.BytesIn, r.BytesOut = in, int64(len(first))+out
	p.Log(r)
}

func (p *Proxy) where() string {
	if p.Where != "" {
		return p.Where
	}
	return "on the Windows host"
}

func (p *Proxy) resolve(ctx context.Context, host string) []netip.Addr {
	if p.Resolve != nil {
		return p.Resolve(ctx, host)
	}
	return Resolve(ctx, host)
}

func (p *Proxy) local() []netip.Addr {
	if p.Local != nil {
		return p.Local()
	}
	return LocalAddrs()
}

func (p *Proxy) dial(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, to)
	}
	d := net.Dialer{Timeout: dialTimeout}
	return d.DialContext(ctx, "tcp", to.String())
}

// Resolve is host's addresses (a name or a literal) from the system
// resolver, IPv4 first.
func Resolve(ctx context.Context, host string) []netip.Addr {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		a = a.Unmap().WithZone("")
		switch {
		case slices.Contains(v4, a) || slices.Contains(v6, a):
		case a.Is4():
			v4 = append(v4, a)
		default:
			v6 = append(v6, a)
		}
	}
	return append(v4, v6...)
}

// LocalAddrs is every address of the host's interfaces, without zones.
func LocalAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var local []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				local = append(local, ip.Unmap())
			}
		}
	}
	return local
}

func reply(c net.Conn, status, body string) { replyType(c, status, "text/plain", body) }

func replyType(c net.Conn, status, typ, body string) {
	io.WriteString(c, "HTTP/1.1 "+status+"\r\nContent-Type: "+typ+"\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
}

// readHead is the request head (through the blank line) and whatever came
// after it, within maxHead bytes and headTimeout.
func readHead(c net.Conn) (head, rest []byte, ok bool) {
	c.SetReadDeadline(time.Now().Add(headTimeout))
	defer c.SetReadDeadline(time.Time{})
	var data []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		data = append(data, buf[:n]...)
		if end := bytes.Index(data, []byte("\r\n\r\n")); end >= 0 {
			end += 4
			if end > maxHead {
				return nil, nil, false
			}
			return data[:end], data[end:], true
		}
		if err != nil || len(data) > maxHead {
			return nil, nil, false
		}
	}
}

const tokenChars = "abcdefghijklmnopqrstuvwxyz0123456789!#$%&'*+-.^_`|~"

// parse reads a CONNECT or an absolute-form http:// request.
func parse(head []byte) (request, bool) {
	if !utf8.Valid(head) {
		return request{}, false
	}
	lines := strings.Split(string(head), "\r\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return request{}, false
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 || parts[2] != "HTTP/1.0" && parts[2] != "HTTP/1.1" {
		return request{}, false
	}
	method, tgt := parts[0], parts[1]
	if method == "" || strings.ContainsFunc(method, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') }) {
		return request{}, false
	}
	if method == "CONNECT" {
		host, port, ok := authority(tgt)
		if !ok || port == 0 {
			return request{}, false
		}
		return request{host: host, port: port}, true
	}
	if len(tgt) < 7 || !strings.EqualFold(tgt[:7], "http://") {
		return request{}, false
	}
	after := tgt[7:]
	end := strings.IndexAny(after, "/?")
	if end < 0 {
		end = len(after)
	}
	host, port, ok := authority(after[:end])
	if !ok {
		return request{}, false
	}
	if port == 0 {
		port = 80
	}
	path := after[end:]
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var headers []string
	hosts := 0
	for _, line := range lines[1:] {
		// No folded lines and no space before the colon, so every header
		// name is the one compared below.
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return request{}, false
		}
		name := strings.ToLower(line[:colon])
		if name == "" || strings.ContainsFunc(name, func(r rune) bool { return !strings.ContainsRune(tokenChars, r) }) {
			return request{}, false
		}
		if strings.HasPrefix(name, "proxy-") || name == "connection" {
			continue
		}
		if name == "host" {
			// The upstream serves the site the header names: it must be the
			// target, or an allowed address would front for any other.
			hosts++
			named, namedPort, ok := authority(strings.Trim(line[colon+1:], " \t"))
			if namedPort == 0 {
				namedPort = 80
			}
			if !ok || named != host || namedPort != port {
				return request{}, false
			}
		}
		headers = append(headers, line)
	}
	if hosts > 1 {
		return request{}, false
	}
	headers = append(headers, "Connection: close")
	up := strings.Join(append([]string{method + " " + path + " " + parts[2]}, headers...), "\r\n") + "\r\n\r\n"
	return request{host: host, port: port, upstreamHead: []byte(up)}, true
}

// authority parses host[:port] or [v6][:port]; the host must be a DNS name
// or an IP literal. Port 0 means none was given.
func authority(text string) (host string, port int, ok bool) {
	var portText string
	hasPort := false
	if strings.HasPrefix(text, "[") {
		end := strings.IndexByte(text, ']')
		if end < 0 {
			return "", 0, false
		}
		host = text[1:end]
		rest := text[end+1:]
		if strings.HasPrefix(rest, ":") {
			portText, hasPort = rest[1:], true
		} else if rest != "" {
			return "", 0, false
		}
		if _, ok := parseAddr(host); !ok {
			return "", 0, false
		}
	} else {
		pieces := strings.Split(text, ":")
		if len(pieces) > 2 {
			return "", 0, false
		}
		host = pieces[0]
		if len(pieces) == 2 {
			portText, hasPort = pieces[1], true
		}
		if host == "" || len(host) > 253 || strings.ContainsFunc(host, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_')
		}) {
			return "", 0, false
		}
	}
	host = normName(host)
	if host == "" {
		return "", 0, false
	}
	if !hasPort {
		return host, 0, true
	}
	n, ok := parsePort(portText)
	if !ok {
		return "", 0, false
	}
	return host, n, true
}

type closeWriter interface{ CloseWrite() error }

// bridge copies both ways until both directions end, and returns the bytes
// copied guest to upstream (out) and back (in), and whether upstream reset the
// connection.
func bridge(guest, up net.Conn) (out, in int64, upstreamReset bool) {
	copyHalf := func(dst net.Conn, src *resetReader) int64 {
		n, _ := io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		return n
	}
	fromUp := &resetReader{r: up}
	var wg sync.WaitGroup
	wg.Go(func() { in = copyHalf(guest, fromUp) })
	out = copyHalf(up, &resetReader{r: guest})
	wg.Wait()
	return out, in, fromUp.reset
}

// resetReader remembers whether reading r ended in a connection reset.
type resetReader struct {
	r     io.Reader
	reset bool
}

func (r *resetReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && isReset(err) {
		r.reset = true
	}
	return n, err
}

// isReset reports a connection reset: ECONNRESET, or WSAECONNRESET (10054) on
// Windows, where Go's syscall.ECONNRESET is a value of its own that Winsock
// never returns.
func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.Errno(10054))
}

// LogTo is a Log for p that writes each record to the VM's log file at
// path and, as a line, to glovd's log.
func LogTo(vm, path string) func(Record) {
	return func(r Record) {
		slog.Info("egress "+r.Summary(), "vm", vm)
		Append(path, LogCap, r)
	}
}

// prefixed is a connection whose reads come from r: bytes already read
// from it, then the rest.
type prefixed struct {
	net.Conn
	r io.Reader
}

func (p prefixed) Read(b []byte) (int, error) { return p.r.Read(b) }
