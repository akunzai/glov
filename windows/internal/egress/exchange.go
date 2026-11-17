package egress

import (
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// answerTimeout bounds reading a token endpoint's whole answer.
const answerTimeout = 60 * time.Second

// exchangeToken serves a POST to a subscription's token endpoint: reads the
// body whole, puts the value of each placeholder that is a whole value in
// it (under the same grant and host checks as a header), sends it, and
// reads the whole answer. In a 2xx JSON answer each field endpoint.Answer
// maps is kept as that secret and replaced by its placeholder; any value
// the proxy put into the request is masked out of the answer. A token never
// reaches the guest: an answer that cannot be read or kept is a 502.
func (p *Proxy) exchangeToken(guest net.Conn, req request, addrs []netip.Addr, settings Settings, defined []Secret,
	plan Plan, endpoint TokenEndpoint, rest []byte, r *Record, refuse func(*Refusal)) {
	header := func(name string) (string, bool) {
		i := slices.IndexFunc(plan.Headers, func(h Header) bool { return strings.EqualFold(h.Name, name) })
		if i < 0 {
			return "", false
		}
		return plan.Headers[i].Value, true
	}
	if _, chunked := header("transfer-encoding"); chunked {
		refuse(&Refusal{Status: 411, Message: "a token request needs a Content-Length", Reason: "token request without length"})
		return
	}
	// One length of digits only, so the proxy reads the body the upstream
	// would have read.
	length := 0
	if n := slices.IndexFunc(plan.Headers, func(h Header) bool { return strings.EqualFold(h.Name, "content-length") }); n >= 0 {
		v := plan.Headers[n].Value
		if v == "" || strings.Trim(v, "0123456789") != "" || slices.ContainsFunc(plan.Headers[n+1:], func(h Header) bool { return strings.EqualFold(h.Name, "content-length") }) {
			refuse(&Refusal{Status: 411, Message: "a token request needs one Content-Length", Reason: "token request without length"})
			return
		}
		var err error
		if length, err = strconv.Atoi(v); err != nil {
			length = maxTokenBody + 1
		}
	}
	if length > maxTokenBody {
		refuse(&Refusal{Status: 413, Message: "token request body over " + strconv.Itoa(maxTokenBody) + " bytes", Reason: "token request too large"})
		return
	}
	format := formatNone
	if t, ok := header("content-type"); ok {
		format = tokenFormatOf(t)
	}
	// The body changes length, and the answer is read whole, so it must
	// come unencoded.
	plan.Headers = slices.DeleteFunc(slices.Clone(plan.Headers), func(h Header) bool {
		return strings.EqualFold(h.Name, "content-length") || strings.EqualFold(h.Name, "accept-encoding")
	})
	if len(rest) < length {
		more := make([]byte, length-len(rest))
		guest.SetReadDeadline(time.Now().Add(headTimeout))
		_, err := io.ReadFull(guest, more)
		guest.SetReadDeadline(time.Time{})
		if err != nil {
			refuse(&Refusal{Status: 408, Message: "no request body within " + headTimeout.String(), Reason: "request timeout"})
			return
		}
		rest = append(rest, more...)
	}
	body := rest[:length]
	var names []string
	if format != formatNone {
		names = tokenNames(body, format)
	}
	if f := Check(names, req.host, req.port, p.VM, settings.Secrets, defined, p.where()); f != nil {
		refuse(f)
		return
	}
	for _, n := range names {
		if !slices.Contains(plan.Secrets, n) {
			plan.Secrets = append(plan.Secrets, n)
		}
	}
	if len(plan.Secrets) > 0 {
		r.Secrets = slices.Clone(plan.Secrets)
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
	content := body
	if format != formatNone && len(names) > 0 {
		var ok bool
		if content, ok = substituteToken(body, format, values); !ok {
			refuse(&Refusal{Status: 400, Message: "the token request body does not parse", Reason: "malformed token request"})
			return
		}
	}
	// Before the closing Connection: close.
	plan.Headers = slices.Insert(plan.Headers, len(plan.Headers)-1, Header{"Content-Length", strconv.Itoa(len(content))})
	head, f := plan.Apply(values)
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
	if _, err := up.Write(append(head, content...)); err != nil {
		refuse(&Refusal{Status: 502, Message: "upstream: " + err.Error(), Reason: "upstream unusable"})
		return
	}
	// A close without close_notify ends the answer as a close would.
	up.SetReadDeadline(time.Now().Add(answerTimeout))
	answer, _ := io.ReadAll(io.LimitReader(up, maxTokenAnswer+1))
	fail := func(status int, reason, message string) {
		refuse(&Refusal{Status: status, Message: message, Reason: reason})
	}
	if len(answer) > maxTokenAnswer {
		fail(502, "token answer too large", "the token endpoint's answer could not be read")
		return
	}
	response, ok := parseTokenResponse(answer)
	if !ok {
		fail(502, "token answer unreadable", "the token endpoint's answer could not be read")
		return
	}
	r.Status = response.Status
	// An error that echoes the request would hand the guest what the proxy
	// put in it.
	response.Body = maskTokens(values, response.Body)
	if response.Status < 200 || response.Status > 299 || len(endpoint.Answer) == 0 {
		sendAnswer(guest, response.framed())
		return
	}
	rewritten, kept, ok := keepTokens(response.Body, endpoint.Answer)
	if !ok {
		fail(502, "token answer not JSON", "the token endpoint's answer is not a JSON object")
		return
	}
	for _, k := range kept {
		if !slices.Contains(r.Secrets, k.Secret.Name) {
			r.Secrets = append(r.Secrets, k.Secret.Name)
		}
	}
	for _, k := range kept {
		var err error = errNoKeep(k.Secret.Name)
		if p.Keep != nil {
			err = p.Keep(k.Secret.Name, k.Secret.Hosts, k.Value)
		}
		if err != nil {
			fail(502, "token not kept", "cannot keep the tokens: "+err.Error())
			return
		}
	}
	response.Body = rewritten
	sendAnswer(guest, response.framed())
}

// sendAnswer writes the whole answer to the guest and ends its side.
func sendAnswer(guest net.Conn, answer []byte) {
	guest.Write(answer)
	if cw, ok := guest.(closeWriter); ok {
		cw.CloseWrite()
	}
}

type errNoKeep string

func (e errNoKeep) Error() string { return "cannot keep secret " + string(e) }
