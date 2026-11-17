// Package egress is glovd's side of an isolated VM's egress proxy
// (docs/guest-protocol.md "Isolated VM egress proxy"): the allowlist, the
// proxy that serves each guest connection on hv_sock port 4024, and the
// per-VM connection log. The guest is assumed hostile, so every decision is
// made here, on names and addresses the host checked itself. It mirrors the
// macOS daemon's EgressPolicy, EgressProxy and EgressLog.
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// DefaultPorts are what an entry without a port allows.
var DefaultPorts = []int{443, 80}

type hostKind int

const (
	kindName hostKind = iota
	kindSuffix
	kindAddr
)

type entry struct {
	kind  hostKind
	name  string     // kindName: the name; kindSuffix: the suffix
	addr  netip.Addr // kindAddr
	ports []int
}

// Policy is a VM's allowlist (vm.json egress.allow). An entry is a hostname,
// *.suffix or an IP literal, optionally with :port ([v6]:port for IPv6);
// without a port it allows 443 and 80 only. Nothing else is allowed.
type Policy struct{ entries []entry }

// NewPolicy parses allow, skipping invalid entries.
func NewPolicy(allow []string) Policy {
	var p Policy
	for _, s := range allow {
		if e, ok := parseEntry(s); ok {
			p.entries = append(p.entries, e)
		}
	}
	return p
}

// Allows reports whether the guest may connect to host (a name or an IP
// literal, as the request named it) on port.
func (p Policy) Allows(host string, port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	addr, isAddr := parseAddr(host)
	name := normName(host)
	if !isAddr && name == "" {
		return false
	}
	for _, e := range p.entries {
		if !slices.Contains(e.ports, port) {
			continue
		}
		switch {
		case isAddr && e.kind == kindAddr && e.addr == addr:
			return true
		case !isAddr && e.kind == kindName && e.name == name:
			return true
		case !isAddr && e.kind == kindSuffix && strings.HasSuffix(name, "."+e.name):
			return true
		}
	}
	return false
}

// Reachable reports whether the proxy may connect to addr: a public unicast
// address that is none of the host's own (local). Loopback, private, CGNAT,
// link-local, multicast, unspecified and reserved ranges are not.
// privateNetworks (a --private entry) also admits RFC 1918, CGNAT and
// unique local addresses; the rest stay out.
func Reachable(addr netip.Addr, local []netip.Addr, privateNetworks bool) bool {
	// An IPv4-mapped address is the IPv4 address it carries, compared as
	// such with the host's own.
	addr = addr.WithZone("").Unmap()
	for _, l := range local {
		if l.WithZone("").Unmap() == addr {
			return false
		}
	}
	if addr.Is4() {
		return publicV4(addr.As4()) || privateNetworks && privateV4(addr.As4())
	}
	b := addr.As16()
	switch {
	case b[0] == 0xff: // multicast
		return false
	case b[0]&0xfe == 0xfc: // fc00::/7 unique local
		return privateNetworks
	case b[0] == 0xfe && b[1]&0xc0 == 0x80: // fe80::/10 link-local
		return false
	case allZero(b[:12]): // ::, ::1 and IPv4-compatible
		return false
	// NAT64 and 6to4 never carry private IPv4 (RFC 6052, RFC 3056), not
	// even for a --private entry.
	case slices.Equal(b[:12], []byte{0x00, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0}): // NAT64
		return publicV4([4]byte(b[12:]))
	case b[0]&0xe0 != 0x20: // outside 2000::/3 global unicast: 64:ff9b:1::/48 local NAT64, fec0::/10, 100::/64, unassigned
		return false
	case b[0] == 0x20 && b[1] == 0x02: // 6to4
		return publicV4([4]byte(b[2:6]))
	case slices.Equal(b[:4], []byte{0x20, 0x01, 0x0d, 0xb8}) || slices.Equal(b[:4], []byte{0x20, 0x01, 0, 0}): // documentation, Teredo
		return false
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && b[3] >= 0x10 && b[3] <= 0x2f: // 2001:10::/28 and 2001:20::/28 ORCHID
		return false
	case b[0] == 0x3f && b[1] == 0xff && b[2]&0xf0 == 0: // 3fff::/20 documentation
		return false
	}
	return true
}

func allZero(b []byte) bool { return !slices.ContainsFunc(b, func(x byte) bool { return x != 0 }) }

// privateV4 is RFC 1918 and CGNAT (100.64.0.0/10, which tailnets use).
func privateV4(b [4]byte) bool {
	return b[0] == 10 || b[0] == 172 && b[1] >= 16 && b[1] <= 31 || b[0] == 192 && b[1] == 168 || b[0] == 100 && b[1] >= 64 && b[1] <= 127
}

func publicV4(b [4]byte) bool {
	switch {
	case b[0] == 0, b[0] == 10, b[0] == 127:
		return false
	case b[0] == 100 && b[1] >= 64 && b[1] <= 127:
		return false
	case b[0] == 169 && b[1] == 254:
		return false
	case b[0] == 172 && b[1] >= 16 && b[1] <= 31:
		return false
	case b[0] == 192 && b[1] == 0 && (b[2] == 0 || b[2] == 2), b[0] == 192 && b[1] == 88 && b[2] == 99, b[0] == 192 && b[1] == 168:
		return false
	case b[0] == 198 && (b[1] == 18 || b[1] == 19), b[0] == 198 && b[1] == 51 && b[2] == 100, b[0] == 203 && b[1] == 0 && b[2] == 113:
		return false
	case b[0] >= 224: // multicast, reserved and broadcast
		return false
	}
	return true
}

// parseAddr parses a numeric IPv4 or IPv6 address (brackets allowed), without
// a zone.
func parseAddr(s string) (netip.Addr, bool) {
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a, true
}

func normName(host string) string { return strings.TrimSuffix(strings.ToLower(host), ".") }

func parsePort(s string) (int, bool) {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

func parseEntry(text string) (entry, bool) {
	host := strings.TrimSpace(text)
	ports := DefaultPorts
	if end := strings.IndexByte(host, ']'); strings.HasPrefix(host, "[") && end >= 0 {
		rest := host[end+1:]
		if strings.HasPrefix(rest, ":") {
			port, ok := parsePort(rest[1:])
			if !ok {
				return entry{}, false
			}
			ports = []int{port}
		} else if rest != "" {
			return entry{}, false
		}
		host = host[1:end]
	} else if strings.Count(host, ":") == 1 {
		i := strings.LastIndexByte(host, ':')
		port, ok := parsePort(host[i+1:])
		if !ok {
			return entry{}, false
		}
		ports = []int{port}
		host = host[:i]
	}
	if a, ok := parseAddr(host); ok {
		return entry{kind: kindAddr, addr: a, ports: ports}, true
	}
	name := normName(host)
	if suffix, ok := strings.CutPrefix(name, "*."); ok {
		return entry{kind: kindSuffix, name: suffix, ports: ports}, isName(suffix)
	}
	return entry{kind: kindName, name: name, ports: ports}, isName(name)
}

// isName reports a lowercased DNS name: dot-separated, non-empty labels of
// letters, digits, '-' and '_'.
func isName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || strings.ContainsFunc(label, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) {
			return false
		}
	}
	return true
}

// Canonical is entry as vm.json stores it (lowercase, no trailing dot,
// compressed IPv6, a port only when it is not the default), or false when
// it is no valid entry.
func Canonical(text string) (string, bool) {
	e, ok := parseEntry(text)
	if !ok {
		return "", false
	}
	port := ""
	if !slices.Equal(e.ports, DefaultPorts) {
		port = ":" + strconv.Itoa(e.ports[0])
	}
	switch e.kind {
	case kindName:
		return e.name + port, true
	case kindSuffix:
		return "*." + e.name + port, true
	}
	a := e.addr.String()
	if port != "" && e.addr.Is6() {
		return "[" + a + "]" + port, true
	}
	return a + port, true
}

// ErrInvalid and ErrNotListed are what Allowing and Removing fail with.
var (
	ErrInvalid   = errors.New("not a valid egress entry")
	ErrNotListed = errors.New("not in the allowlist")
)

// Allowing is allow with entries added in canonical form, each once; it
// fails on any invalid entry.
func Allowing(allow, entries []string) ([]string, error) {
	var invalid, forms []string
	for _, e := range entries {
		if f, ok := Canonical(e); ok {
			forms = append(forms, f)
		} else {
			invalid = append(invalid, strconv.Quote(e))
		}
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: %s (use a hostname, *.suffix or an IP address, optionally with :port)", ErrInvalid, strings.Join(invalid, ", "))
	}
	result := slices.Clone(allow)
	for _, f := range forms {
		if !slices.Contains(result, f) {
			result = append(result, f)
		}
	}
	return result, nil
}

// Removing is allow without entries, compared in canonical form; it fails
// when one is not listed.
func Removing(allow, entries []string) ([]string, error) {
	form := func(s string) string {
		if f, ok := Canonical(s); ok {
			return f
		}
		return s
	}
	listed := make([]string, len(allow))
	for i, a := range allow {
		listed[i] = form(a)
	}
	var missing []string
	drop := map[string]bool{}
	for _, e := range entries {
		f := form(e)
		if !slices.Contains(listed, f) {
			missing = append(missing, e)
		}
		drop[f] = true
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotListed, strings.Join(missing, ", "))
	}
	result := []string{}
	for i, a := range allow {
		if !drop[listed[i]] {
			result = append(result, a)
		}
	}
	return result, nil
}

// Settings are an isolated VM's egress settings, vm.json's egress object
// with the macOS keys.
type Settings struct {
	// Allow is the allowlist (Policy), in canonical form.
	Allow []string `json:"allow"`
	// MCPEdits lets MCP clients change Allow; only glov egress mcp sets it.
	MCPEdits bool `json:"mcpEdits,omitzero"`
	// Secrets names the secrets granted to the VM (glov secret grant):
	// names only, never a value.
	Secrets []string `json:"secrets,omitempty"`
	// PrivateAllow are the Allow entries (exact names or addresses) that
	// may resolve to private-network addresses (glov egress allow
	// --private); only the CLI adds them.
	PrivateAllow []string `json:"privateAllow,omitempty"`
	// Trust are CA certificates (glov vm trust) the guest's trust store
	// holds and the proxy verifies intercepted upstreams with, besides the
	// host's.
	Trust []Trust `json:"trust,omitempty"`
	// Open lets the guest reach any public host, still through the proxy
	// (glov vm configure --egress open); Allow waits for a switch back.
	Open bool `json:"open,omitzero"`
	// PlainSecrets are the tmpfs secrets the owner let the VM hold while
	// open (--allow-plain-secrets); while open, any other is withheld.
	PlainSecrets []string `json:"plainSecrets,omitempty"`
	// Repos are the repositories (RepoPattern texts) the VM may reach on
	// the hosts of its git secrets (glov secret repos); none reaches none.
	Repos []string `json:"repos,omitempty"`
	// Subscriptions are the harnesses signed in to the owner's subscription
	// (glov secret login) instead of using an API key: their token endpoints
	// are intercepted, and the guest is set up for them.
	Subscriptions []string `json:"subscriptions,omitempty"`
}

// Trust is a CA certificate given to a VM: public, so it lives in vm.json.
type Trust struct {
	// Name is 1–64 of A-Za-z0-9._-; the guest file is <name>.crt.
	Name string `json:"name"`
	// Certificate is one PEM certificate, as glovd re-encoded it.
	Certificate string `json:"certificate"`
}

// Permits reports whether the guest may connect to host on port at all:
// any host when open, else an Allow entry's.
func (s Settings) Permits(host string, port int) bool {
	if s.Open {
		_, isAddr := parseAddr(host)
		return port >= 1 && port <= 65535 && (isAddr || isName(normName(host)))
	}
	return NewPolicy(s.Allow).Allows(host, port)
}

// ReachesPrivate reports whether the entry that lets the guest reach host
// on port was allowed with --private.
func (s Settings) ReachesPrivate(host string, port int) bool {
	return len(s.PrivateAllow) > 0 && NewPolicy(s.PrivateAllow).Allows(host, port)
}

// ErrPrivateSuffix is what MarkingPrivate fails with on a *.suffix.
var ErrPrivateSuffix = errors.New("--private takes an exact hostname or IP address")

// MarkingPrivate is marked with entries (already allowed) added in
// canonical form; only exact names and addresses take --private.
func MarkingPrivate(marked, entries []string) ([]string, error) {
	forms, err := Allowing(nil, entries)
	if err != nil {
		return nil, err
	}
	if suffixes := slices.DeleteFunc(slices.Clone(forms), func(f string) bool { return !strings.HasPrefix(f, "*.") }); len(suffixes) > 0 {
		return nil, fmt.Errorf("%w, not %s", ErrPrivateSuffix, strings.Join(suffixes, ", "))
	}
	return Allowing(marked, forms)
}

// Unmarking is marked without entries, compared in canonical form; an
// entry not marked is no error.
func Unmarking(marked, entries []string) []string {
	form := func(s string) string {
		if f, ok := Canonical(s); ok {
			return f
		}
		return s
	}
	drop := map[string]bool{}
	for _, e := range entries {
		drop[form(e)] = true
	}
	out := slices.DeleteFunc(slices.Clone(marked), func(m string) bool { return drop[form(m)] })
	if len(out) == 0 {
		return nil
	}
	return out
}
