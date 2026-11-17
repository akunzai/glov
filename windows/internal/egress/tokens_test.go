package egress

import (
	"encoding/json/v2"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestWholePlaceholderValuesInFormAndJSONBodiesGetTheirValues(t *testing.T) {
	form := []byte("grant_type=refresh_token&refresh_token=glov-secret%3Abox.codex.refresh&note=glov-secret:x+y")
	if got := tokenNames(form, formatForm); !slices.Equal(got, []string{"box.codex.refresh"}) {
		t.Errorf("form names %q", got)
	}
	swapped, ok := substituteToken(form, formatForm, map[string]string{"box.codex.refresh": "r/1 +="})
	if want := "grant_type=refresh_token&refresh_token=r%2F1%20%2B%3D&note=glov-secret:x+y"; !ok || string(swapped) != want {
		t.Errorf("form %q, want %q", swapped, want)
	}

	body := []byte(`{"grant_type":"refresh_token","refresh_token":"glov-secret:box.codex.refresh","n":1,"glov-secret:box.codex.refresh":["glov-secret:box.codex.refresh"]}`)
	if got := tokenNames(body, formatJSON); !slices.Equal(got, []string{"box.codex.refresh"}) {
		t.Errorf("JSON names %q", got)
	}
	swapped, ok = substituteToken(body, formatJSON, map[string]string{"box.codex.refresh": "r1"})
	if want := `{"grant_type":"refresh_token","refresh_token":"r1","n":1,"glov-secret:box.codex.refresh":["r1"]}`; !ok || string(swapped) != want {
		t.Errorf("JSON %s, want %s (a member name stays)", swapped, want)
	}
	for _, bad := range []string{"{", `{"a":1} {"b":2}`, `{"a":1,"a":2}`} {
		if _, ok := substituteToken([]byte(bad), formatJSON, nil); ok {
			t.Errorf("%s parsed", bad)
		}
	}
	for ct, want := range map[string]tokenFormat{"application/json; charset=utf-8": formatJSON, "application/x-www-form-urlencoded": formatForm, "application/vnd.x+json": formatJSON, "text/plain": formatNone} {
		if got := tokenFormatOf(ct); got != want {
			t.Errorf("%s: %v", ct, got)
		}
	}
}

func TestAChunkedAnswerIsReadWholeAndReframed(t *testing.T) {
	r, ok := parseTokenResponse([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Type: application/json\r\n\r\n5\r\n{\"a\":\r\n4\r\n\"b\"}\r\n0\r\n\r\n"))
	if !ok || r.Status != 200 || string(r.Body) != `{"a":"b"}` {
		t.Fatalf("%+v, %v", r, ok)
	}
	if got := string(r.framed()); got != "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 9\r\nConnection: close\r\n\r\n{\"a\":\"b\"}" {
		t.Errorf("framed %q", got)
	}
	for _, bad := range []string{"HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort", "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\nx", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n"} {
		if _, ok := parseTokenResponse([]byte(bad)); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestTheAnswerKeepsDeclaredTokensAndGivesPlaceholders(t *testing.T) {
	answer := map[string]KeptSecret{
		"access_token":  {"box.codex.access", []string{"chatgpt.com"}},
		"refresh_token": {"box.codex.refresh", []string{"auth.openai.com"}},
	}
	body, kept, ok := keepTokens([]byte(`{"access_token":"A","id_token":"I","expires_in":3600,"x":{"refresh_token":"inner"}}`), answer)
	if !ok || len(kept) != 1 || kept[0].Secret.Name != "box.codex.access" || kept[0].Value != "A" {
		t.Fatalf("kept %+v, %v", kept, ok)
	}
	if want := `{"access_token":"glov-secret:box.codex.access","id_token":"I","expires_in":3600,"x":{"refresh_token":"inner"}}`; string(body) != want {
		t.Errorf("body %s, want %s", body, want)
	}
	if _, _, ok := keepTokens([]byte("[]"), answer); ok {
		t.Error("an array was kept")
	}
	if got := string(maskTokens(map[string]string{"a": "SECRET", "b": "SECRET-LONGER"}, []byte("x SECRET-LONGER SECRET"))); got != "x glov-secret:b glov-secret:a" {
		t.Errorf("masked %q", got)
	}
}

// TestSignInAndRefreshKeepTheTokensOnTheHostAndTheGuestSeesPlaceholders is
// macOS's TokenExchangeTests case of that name.
func TestSignInAndRefreshKeepTheTokensOnTheHostAndTheGuestSeesPlaceholders(t *testing.T) {
	u, ca := newUpstream(t), egressCA(t)
	var mu sync.Mutex
	round := 0
	u.respond = func(head, body string) string {
		mu.Lock()
		round++
		n := round
		mu.Unlock()
		if strings.Contains(body, "bad") {
			return "HTTP/1.1 401 Unauthorized\r\nContent-Length: 4\r\nConnection: close\r\n\r\nnope"
		}
		if strings.Contains(body, "echo") {
			b := "invalid: " + body
			return "HTTP/1.1 400 Bad Request\r\nContent-Length: " + strconv.Itoa(len(b)) + "\r\nConnection: close\r\n\r\n" + b
		}
		b := `{"access_token":"A` + strconv.Itoa(n) + `REAL","refresh_token":"R` + strconv.Itoa(n) + `REAL","id_token":"I"}`
		return "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" + strconv.FormatInt(int64(len(b)), 16) + "\r\n" + b + "\r\n0\r\n\r\n"
	}
	type stored struct {
		hosts []string
		value string
	}
	store := map[string]stored{}
	failing := false
	tp := newTLSProxy(t, u, ca)
	tp.Settings = func() (Settings, error) {
		return Settings{Allow: []string{"api.example.com", "chatgpt.com"}, Secrets: []string{"box.codex.access", "box.codex.refresh"}, Subscriptions: []string{"codex"}}, nil
	}
	tp.Secrets = func() ([]Secret, error) {
		mu.Lock()
		defer mu.Unlock()
		var out []Secret
		for n, s := range store {
			out = append(out, Secret{Name: n, Hosts: s.hosts})
		}
		return out, nil
	}
	tp.Value = func(name string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		s, ok := store[name]
		if !ok {
			return "", errors.New("no " + name)
		}
		return s.value, nil
	}
	tp.TokenEndpoints = func(subs []string) []TokenEndpoint {
		if !slices.Equal(subs, []string{"codex"}) {
			t.Errorf("subscriptions %q", subs)
		}
		return []TokenEndpoint{{Host: "api.example.com", Path: "/oauth/token", Answer: map[string]KeptSecret{
			"access_token":  {"box.codex.access", []string{"chatgpt.com"}},
			"refresh_token": {"box.codex.refresh", []string{"api.example.com"}},
		}}, {Host: "elsewhere.example.com", Path: "/oauth/token"}}
	}
	tp.Keep = func(name string, hosts []string, value string) error {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return errors.New("the store is locked")
		}
		store[name] = stored{hosts, value}
		return nil
	}
	post := func(body, contentType string) string {
		return "POST /oauth/token?x=1 HTTP/1.1\r\nHost: api.example.com\r\nContent-Type: " + contentType + "\r\nAccept-Encoding: gzip\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	const form = "application/x-www-form-urlencoded"

	// Sign-in: no secret exists yet, the code goes up as it is, and the
	// tokens of the answer stay here.
	signIn, err := tp.exchange(t, post("grant_type=authorization_code&code=c1", form), ca)
	if err != nil || !strings.HasPrefix(signIn, "HTTP/1.1 200 OK") || strings.Contains(signIn, "REAL") ||
		!strings.Contains(signIn, `"access_token":"glov-secret:box.codex.access"`) || !strings.Contains(signIn, `"id_token":"I"`) {
		t.Fatalf("sign-in %q, %v", signIn, err)
	}
	if store["box.codex.access"].value != "A1REAL" || !slices.Equal(store["box.codex.refresh"].hosts, []string{"api.example.com"}) {
		t.Errorf("stored %v", store)
	}
	u.mu.Lock()
	first := u.heads[0] + u.bodies[0]
	u.mu.Unlock()
	if !strings.HasSuffix(first, "Content-Length: 37\r\nConnection: close\r\n\r\ngrant_type=authorization_code&code=c1") || strings.Contains(first, "gzip") {
		t.Errorf("upstream got %q", first)
	}

	// Refresh: the real refresh token goes up in the body, and the rotated
	// one replaces it here.
	refreshBody, _ := json.Marshal(map[string]string{"grant_type": "refresh_token", "refresh_token": "glov-secret:box.codex.refresh"})
	refresh, _ := tp.exchange(t, post(string(refreshBody), "application/json"), ca)
	if !strings.Contains(refresh, `"refresh_token":"glov-secret:box.codex.refresh"`) || strings.Contains(refresh, "REAL") {
		t.Errorf("refresh %q", refresh)
	}
	u.mu.Lock()
	last := u.bodies[len(u.bodies)-1]
	u.mu.Unlock()
	if !strings.Contains(last, `"refresh_token":"R1REAL"`) || store["box.codex.refresh"].value != "R2REAL" {
		t.Errorf("upstream got %q; stored %v", last, store)
	}

	// A refusal passes through untouched; a token that cannot be kept never
	// reaches the guest.
	if refused, _ := tp.exchange(t, post("code=bad", form), ca); !strings.HasPrefix(refused, "HTTP/1.1 401") || !strings.HasSuffix(refused, "nope") {
		t.Errorf("refused %q", refused)
	}
	echoed, _ := tp.exchange(t, post("echo=1&refresh_token=glov-secret%3Abox.codex.refresh", form), ca)
	if !strings.HasPrefix(echoed, "HTTP/1.1 400") || !strings.HasSuffix(echoed, "refresh_token=glov-secret:box.codex.refresh") || strings.Contains(echoed, "REAL") {
		t.Errorf("echoed %q", echoed)
	}
	mu.Lock()
	failing = true
	mu.Unlock()
	if unkept, _ := tp.exchange(t, post("code=c4", form), ca); !strings.HasPrefix(unkept, "HTTP/1.1 502") || strings.Contains(unkept, "REAL") {
		t.Errorf("unkept %q", unkept)
	}
	if big, _ := tp.exchange(t, "POST /oauth/token HTTP/1.1\r\nHost: api.example.com\r\nContent-Length: 70000\r\n\r\n", ca); !strings.HasPrefix(big, "HTTP/1.1 413") {
		t.Errorf("too large %q", big)
	}
	if chunked, _ := tp.exchange(t, "POST /oauth/token HTTP/1.1\r\nHost: api.example.com\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", ca); !strings.HasPrefix(chunked, "HTTP/1.1 411") {
		t.Errorf("chunked %q", chunked)
	}

	lines, _ := tp.snapshot()
	if len(lines) != 7 || !strings.Contains(lines[0], "POST /oauth/token 200") || !strings.HasSuffix(lines[0], "secrets=box.codex.access,box.codex.refresh") ||
		!strings.HasSuffix(lines[4], "(token not kept)") {
		t.Errorf("log %q", lines)
	}
	if strings.Contains(strings.Join(lines, ""), "REAL") {
		t.Error("the log holds a token")
	}
}
