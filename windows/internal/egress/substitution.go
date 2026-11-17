package egress

import (
	"encoding/base64"
	"slices"
	"strings"
	"unicode/utf8"
)

// Placeholder starts the text a guest writes where a credential goes:
// glov-secret:<name> (docs/networking.md "Secrets"). The proxy puts the
// value there, in header values only, and only for a secret granted to the
// VM and bound to the host the request goes to.
const Placeholder = "glov-secret:"

// Secret is a secret as the proxy needs it: its name, the hosts it may be
// sent to (Policy entries in canonical form) and, for a git sign-in, its
// forge. Never its value.
type Secret struct {
	Name  string
	Hosts []string
	// Forge makes the secret a git sign-in: its requests are held to the
	// VM's repositories (Settings.Repos).
	Forge Forge
}

// Refusal is why a request is answered without going upstream.
type Refusal struct {
	Status  int
	Message string
	// Reason is for egress.log: it names a secret, never a value.
	Reason string
}

func (r *Refusal) Error() string { return r.Message }

// Plan is a request head checked against the VM's grants, waiting for the
// values of Secrets.
type Plan struct {
	Method string
	// Path is the request target without its query, for the log.
	Path        string
	RequestLine string
	Headers     []Header
	// Secrets are the names the head uses, in order of first use.
	Secrets []string
}

// Header is one request header.
type Header struct{ Name, Value string }

const nameChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

func isNameChar(c byte) bool { return strings.IndexByte(nameChars, c) >= 0 }

// placeholderNames is the names of the placeholders in text, in order,
// duplicates kept.
func placeholderNames(text string) []string {
	var found []string
	for {
		i := strings.Index(text, Placeholder)
		if i < 0 {
			return found
		}
		text = text[i+len(Placeholder):]
		n := 0
		for n < len(text) && isNameChar(text[n]) {
			n++
		}
		found = append(found, text[:n])
	}
}

// MentionsPlaceholder reports whether head, a plain-HTTP request, names a
// placeholder anywhere, Basic credentials included: such a request is
// refused, since a value is never sent unencrypted.
func MentionsPlaceholder(head []byte) bool {
	text := string(head)
	if strings.Contains(text, Placeholder) {
		return true
	}
	for line := range strings.SplitSeq(text, "\r\n") {
		if c, ok := basicLine(line); ok && strings.Contains(c, Placeholder) {
			return true
		}
	}
	return false
}

// basicLine is the decoded user:password of an Authorization: Basic header
// line.
func basicLine(line string) (string, bool) {
	name, value, ok := strings.Cut(line, ":")
	if !ok || !strings.EqualFold(name, "authorization") {
		return "", false
	}
	return basic(strings.Trim(value, " \t"))
}

// basic is the decoded credentials of an Authorization: Basic value.
func basic(value string) (string, bool) {
	scheme, cred, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "basic") {
		return "", false
	}
	data, err := base64.StdEncoding.DecodeString(strings.Trim(cred, " \t"))
	if err != nil || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

// NewPlan checks an origin-form request head bound for host:port against
// the VM's grants: granted are the names in vm.json, defined every secret
// on the host. mention names where the owner defines and grants secrets,
// such as "on the Windows host".
func NewPlan(head []byte, host string, port int, vm string, granted []string, defined []Secret, where string) (Plan, *Refusal) {
	bad := func(message string) (Plan, *Refusal) {
		return Plan{}, &Refusal{Status: 400, Message: message, Reason: "malformed request"}
	}
	if !utf8.Valid(head) {
		return bad("the request head is not UTF-8")
	}
	// Lines end in CRLF only: a bare CR or LF (or a NUL) an upstream might
	// read as a line end would hide a header, a second Host among them,
	// from the checks below.
	if strings.ContainsAny(strings.ReplaceAll(strings.TrimSuffix(string(head), "\r\n\r\n"), "\r\n", ""), "\r\n\x00") {
		return bad("a bare CR, LF or NUL in the head")
	}
	lines := strings.Split(string(head), "\r\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return bad("empty request")
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 || parts[2] != "HTTP/1.0" && parts[2] != "HTTP/1.1" || !strings.HasPrefix(parts[1], "/") {
		return bad("expected an origin-form HTTP/1.x request")
	}
	method := parts[0]
	if method == "" || strings.ContainsFunc(method, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') }) {
		return bad("malformed method")
	}
	var headers []Header
	hosts := 0
	for _, line := range lines[1:] {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return bad("malformed header")
		}
		name := line[:colon]
		lower := strings.ToLower(name)
		if lower == "" || strings.ContainsFunc(lower, func(r rune) bool { return !strings.ContainsRune(tokenChars, r) }) {
			return bad("malformed header")
		}
		value := strings.Trim(line[colon+1:], " \t")
		if strings.HasPrefix(lower, "proxy-") || lower == "connection" || lower == "keep-alive" || lower == "upgrade" {
			continue
		}
		if lower == "host" {
			// The upstream serves the site the header names: on a shared
			// front end, another name could reach a site the secret is not
			// bound to.
			hosts++
			named, namedPort, ok := authority(value)
			if namedPort == 0 {
				namedPort = 443
			}
			if !ok || named != host || namedPort != port {
				return Plan{}, &Refusal{Status: 403, Message: "the Host header must name " + host, Reason: "Host header mismatch"}
			}
		}
		headers = append(headers, Header{name, value})
	}
	if hosts != 1 {
		return bad("send exactly one Host header")
	}
	var secrets []string
	for _, h := range headers {
		names := placeholderNames(h.Value)
		if c, ok := basic(h.Value); ok {
			names = append(names, placeholderNames(c)...)
		}
		for _, n := range names {
			if !slices.Contains(secrets, n) {
				secrets = append(secrets, n)
			}
		}
	}
	if r := Check(secrets, host, port, vm, granted, defined, where); r != nil {
		return Plan{}, r
	}
	headers = append(headers, Header{"Connection", "close"})
	path, _, _ := strings.Cut(parts[1], "?")
	return Plan{Method: method, Path: path, RequestLine: lines[0], Headers: headers, Secrets: secrets}, nil
}

// Check is why the placeholders names may not go to host:port, or nil when
// each is defined, granted and bound to it.
func Check(names []string, host string, port int, vm string, granted []string, defined []Secret, where string) *Refusal {
	for _, name := range names {
		i := slices.IndexFunc(defined, func(s Secret) bool { return s.Name == name })
		if i < 0 {
			return &Refusal{Status: 403, Reason: "secret " + name + " undefined",
				Message: "no secret named " + name + ": define it " + where + " with glov secret set or glov secret source, then glov secret grant " + vm + " " + name}
		}
		if !slices.Contains(granted, name) {
			return &Refusal{Status: 403, Reason: "secret " + name + " not granted",
				Message: "VM " + vm + " was not granted secret " + name + ": run glov secret grant " + vm + " " + name + " " + where}
		}
		if !NewPolicy(defined[i].Hosts).Allows(host, port) {
			return &Refusal{Status: 403, Reason: "secret " + name + " not bound to host",
				Message: "secret " + name + " is not bound to " + host + ": it may go to " + strings.Join(defined[i].Hosts, ", ") + " only"}
		}
	}
	return nil
}

// Apply is the head to send upstream, with each placeholder of p.Secrets
// replaced by its value.
func (p Plan) Apply(values map[string]string) ([]byte, *Refusal) {
	for _, name := range p.Secrets {
		v, ok := values[name]
		if !ok {
			return nil, &Refusal{Status: 502, Message: "secret " + name + " has no value", Reason: "secret " + name + " unavailable"}
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, &Refusal{Status: 502, Message: "secret " + name + ": the value contains a line break or NUL", Reason: "secret " + name + " unusable"}
		}
	}
	// Longest names first, so a never rewrites part of glov-secret:ab.
	order := slices.Clone(p.Secrets)
	slices.SortStableFunc(order, func(a, b string) int { return len(b) - len(a) })
	replace := func(text string) string {
		for _, name := range order {
			text = replaceWhole(text, Placeholder+name, values[name])
		}
		return text
	}
	var b strings.Builder
	b.WriteString(p.RequestLine)
	for _, h := range p.Headers {
		v := h.Value
		if c, ok := basic(v); ok && strings.Contains(c, Placeholder) {
			v = "Basic " + base64.StdEncoding.EncodeToString([]byte(replace(c)))
		} else if strings.Contains(v, Placeholder) {
			v = replace(v)
		}
		b.WriteString("\r\n" + h.Name + ": " + v)
	}
	b.WriteString("\r\n\r\n")
	return []byte(b.String()), nil
}

// replaceWhole is text with each placeholder not followed by another name
// character replaced by value.
func replaceWhole(text, placeholder, value string) string {
	var b strings.Builder
	for {
		i := strings.Index(text, placeholder)
		if i < 0 {
			b.WriteString(text)
			return b.String()
		}
		b.WriteString(text[:i])
		end := i + len(placeholder)
		if end < len(text) && isNameChar(text[end]) {
			b.WriteString(placeholder)
		} else {
			b.WriteString(value)
		}
		text = text[end:]
	}
}
