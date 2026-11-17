package egress

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The token endpoints of a VM's subscriptions (harness profiles'
// subscription.tokenEndpoints): the guest signs in and refreshes there
// itself, the proxy puts the real values into the request body, and the
// tokens of the answer stay on the host, the guest getting placeholders for
// them (docs/networking.md "Subscriptions"). As macOS's TokenExchange.

// Largest token request body and answer taken whole; token requests and
// answers are a few KiB.
const (
	maxTokenBody   = 64 << 10
	maxTokenAnswer = 1 << 20
)

// TokenEndpoint is one token endpoint of a VM.
type TokenEndpoint struct {
	Host string
	// Path is matched against the request path without its query.
	Path string
	// Answer maps an answer field to the secret that keeps it.
	Answer map[string]KeptSecret
}

// KeptSecret is a secret a token answer is kept as, and the hosts it is
// bound to.
type KeptSecret struct {
	Name  string
	Hosts []string
}

type tokenFormat int

const (
	formatNone tokenFormat = iota
	formatForm
	formatJSON
)

func tokenFormatOf(contentType string) tokenFormat {
	t, _, _ := strings.Cut(contentType, ";")
	t = strings.ToLower(strings.TrimSpace(t))
	switch {
	case t == "application/x-www-form-urlencoded":
		return formatForm
	case t == "application/json" || strings.HasSuffix(t, "+json"):
		return formatJSON
	}
	return formatNone
}

// wholePlaceholder is the secret a whole value is the placeholder of.
func wholePlaceholder(v string) (string, bool) {
	name, ok := strings.CutPrefix(v, Placeholder)
	if !ok || name == "" || len(name) > 64 || name == "." || name == ".." {
		return "", false
	}
	for i := range len(name) {
		if !isNameChar(name[i]) {
			return "", false
		}
	}
	return name, true
}

// tokenNames is the placeholder names in a token request body: whole values
// only, after decoding, each once.
func tokenNames(body []byte, f tokenFormat) []string {
	var out []string
	add := func(v string) {
		if n, ok := wholePlaceholder(v); ok && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	switch f {
	case formatForm:
		if !utf8.Valid(body) {
			return nil
		}
		for pair := range strings.SplitSeq(string(body), "&") {
			if _, v, ok := strings.Cut(pair, "="); ok {
				if d, err := url.QueryUnescape(v); err == nil {
					add(d)
				}
			}
		}
	case formatJSON:
		rewriteJSON(body, func(_ int, _ string, v string) (string, bool) { add(v); return "", false })
	}
	return out
}

// substituteToken is body with each value that is exactly a placeholder of
// values replaced; false when the body does not parse.
func substituteToken(body []byte, f tokenFormat, values map[string]string) ([]byte, bool) {
	swap := func(v string) (string, bool) {
		n, ok := wholePlaceholder(v)
		if !ok {
			return "", false
		}
		real, ok := values[n]
		return real, ok
	}
	switch f {
	case formatForm:
		if !utf8.Valid(body) {
			return nil, false
		}
		pairs := strings.Split(string(body), "&")
		for i, pair := range pairs {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			d, err := url.QueryUnescape(v)
			if err != nil {
				continue
			}
			if real, ok := swap(d); ok {
				pairs[i] = k + "=" + formEncode(real)
			}
		}
		return []byte(strings.Join(pairs, "&")), true
	case formatJSON:
		return rewriteJSON(body, func(_ int, _ string, v string) (string, bool) { return swap(v) })
	}
	return body, true
}

// formEncode percent-encodes all but ASCII letters, digits and -._~.
func formEncode(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// rewriteJSON is the JSON document body with each string value (never an
// object member name) replaced where swap says so; swap gets the value's
// depth (1 for a member of the top-level object) and its member name, if
// any. Every other token stays as it came. False when body is not one JSON
// value.
func rewriteJSON(body []byte, swap func(depth int, member, value string) (string, bool)) ([]byte, bool) {
	dec := jsontext.NewDecoder(bytes.NewReader(body))
	var out bytes.Buffer
	enc := jsontext.NewEncoder(&out)
	var members []string // the current member name at each object depth
	values := 0
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false
		}
		depth := dec.StackDepth()
		if depth == 0 {
			// A top-level value ends here; a second one is not one document.
			if values++; values > 1 {
				return nil, false
			}
		}
		kind, n := dec.StackIndex(depth)
		for len(members) < depth {
			members = append(members, "")
		}
		members = members[:depth]
		if tok.Kind() == '"' {
			if kind == '{' && n%2 == 1 {
				members[depth-1] = tok.String()
			} else {
				member := ""
				if kind == '{' {
					member = members[depth-1]
				}
				if v, ok := swap(depth, member, tok.String()); ok {
					tok = jsontext.String(v)
				}
			}
		}
		if err := enc.WriteToken(tok); err != nil {
			return nil, false
		}
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true
}

// maskTokens is body with each occurrence of a value of sent replaced by
// its placeholder, longest values first.
func maskTokens(sent map[string]string, body []byte) []byte {
	names := slices.Collect(maps.Keys(sent))
	slices.SortStableFunc(names, func(a, b string) int {
		if d := len(sent[b]) - len(sent[a]); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	for _, n := range names {
		if v := sent[n]; v != "" {
			body = bytes.ReplaceAll(body, []byte(v), []byte(Placeholder+n))
		}
	}
	return body
}

// keptToken is one token of an answer (its field and value) and the secret
// that keeps it.
type keptToken struct {
	Field  string
	Secret KeptSecret
	Value  string
}

// keepTokens is the tokens of a JSON answer that answer keeps, and the
// answer with placeholders in their place; false when the answer is not a
// JSON object.
func keepTokens(body []byte, answer map[string]KeptSecret) ([]byte, []keptToken, bool) {
	if v := jsontext.Value(bytes.TrimSpace(body)); !v.IsValid() || v.Kind() != '{' {
		return nil, nil, false
	}
	var kept []keptToken
	out, ok := rewriteJSON(body, func(depth int, member, value string) (string, bool) {
		s, keep := answer[member]
		if depth != 1 || !keep || value == "" {
			return "", false
		}
		kept = append(kept, keptToken{member, s, value})
		return Placeholder + s.Name, true
	})
	slices.SortFunc(kept, func(a, b keptToken) int { return strings.Compare(a.Field, b.Field) })
	return out, kept, ok
}

// tokenResponse is a whole HTTP/1.x response as read until the server
// closed: status line, headers and the decoded body.
type tokenResponse struct {
	Status     int
	StatusLine string
	Headers    []Header
	Body       []byte
}

// framed is the response as one message to the guest: the body is decoded,
// so its framing headers are its own again.
func (r tokenResponse) framed() []byte {
	var b bytes.Buffer
	b.WriteString(r.StatusLine)
	for _, h := range r.Headers {
		switch strings.ToLower(h.Name) {
		case "content-length", "transfer-encoding", "content-encoding", "connection":
			continue
		}
		b.WriteString("\r\n" + h.Name + ": " + h.Value)
	}
	b.WriteString("\r\nContent-Length: " + strconv.Itoa(len(r.Body)) + "\r\nConnection: close\r\n\r\n")
	b.Write(r.Body)
	return b.Bytes()
}

// parseTokenResponse reads data as one response; false when it is not
// complete: no head, a short body, a broken chunk, or an encoding other
// than identity.
func parseTokenResponse(data []byte) (tokenResponse, bool) {
	head, rest, ok := bytes.Cut(data, []byte("\r\n\r\n"))
	if !ok || !utf8.Valid(head) {
		return tokenResponse{}, false
	}
	lines := strings.Split(string(head), "\r\n")
	parts := strings.Fields(lines[0])
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return tokenResponse{}, false
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return tokenResponse{}, false
	}
	r := tokenResponse{Status: status, StatusLine: lines[0]}
	for _, l := range lines[1:] {
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			return tokenResponse{}, false
		}
		r.Headers = append(r.Headers, Header{name, strings.TrimSpace(value)})
	}
	header := func(name string) (string, bool) {
		for _, h := range r.Headers {
			if strings.EqualFold(h.Name, name) {
				return h.Value, true
			}
		}
		return "", false
	}
	if e, ok := header("content-encoding"); ok && !strings.EqualFold(e, "identity") {
		return tokenResponse{}, false
	}
	te, _ := header("transfer-encoding")
	cl, hasLength := header("content-length")
	switch {
	case strings.Contains(strings.ToLower(te), "chunked"):
		for {
			line, after, ok := bytes.Cut(rest, []byte("\r\n"))
			if !ok {
				return tokenResponse{}, false
			}
			sizeText, _, _ := strings.Cut(string(line), ";")
			size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
			if err != nil || size < 0 {
				return tokenResponse{}, false
			}
			if size == 0 {
				break
			}
			if int64(len(after)) < size+2 {
				return tokenResponse{}, false
			}
			r.Body = append(r.Body, after[:size]...)
			rest = after[size+2:]
		}
	case hasLength:
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 {
			r.Body = rest
			break
		}
		if len(rest) < n {
			return tokenResponse{}, false
		}
		r.Body = rest[:n]
	default:
		r.Body = rest
	}
	return r, true
}
