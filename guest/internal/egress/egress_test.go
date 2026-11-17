package egress

import (
	"context"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestServePipesEachClientToItsOwnHostConnection checks the bridge moves bytes
// both ways, unparsed, and opens one host connection per client.
func TestServePipesEachClientToItsOwnHostConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hosts := make(chan net.Conn, 2)
	dial := func() (io.ReadWriteCloser, error) {
		guestEnd, hostEnd := net.Pipe()
		hosts <- hostEnd
		return guestEnd, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, dial) }()

	for _, msg := range []string{"CONNECT example.com:443 HTTP/1.1\r\n\r\n", "GET http://example.com/ HTTP/1.1\r\n\r\n"} {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		var host net.Conn
		select {
		case host = <-hosts:
		case <-time.After(5 * time.Second):
			t.Fatal("no host connection")
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(host, got); err != nil || string(got) != msg {
			t.Fatalf("host got %q, %v", got, err)
		}
		if _, err := host.Write([]byte("reply")); err != nil {
			t.Fatal(err)
		}
		host.Close()
		back, err := io.ReadAll(c)
		if err != nil || string(back) != "reply" {
			t.Fatalf("client got %q, %v", back, err)
		}
		c.Close()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

func TestADialFailureClosesTheClient(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, l, func() (io.ReadWriteCloser, error) { return nil, os.ErrNotExist })
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read %d, %v; want EOF", n, err)
	}
}

func TestEnvPointsProxyVariablesAtTheGuestListenerAndCAVariablesAtTheEgressCA(t *testing.T) {
	proxy := map[string]string{
		"HTTP_PROXY": "http://127.0.0.1:3128", "HTTPS_PROXY": "http://127.0.0.1:3128",
		"http_proxy": "http://127.0.0.1:3128", "https_proxy": "http://127.0.0.1:3128",
		"NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1",
	}
	check := func(want map[string]string) {
		t.Helper()
		got := Env()
		if len(got) != len(want) {
			t.Fatalf("Env() = %v", got)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
	}
	defer func(old string) { CAFile = old }(CAFile)

	// No CA (a Windows host, which has none yet): proxy variables only.
	CAFile = filepath.Join(t.TempDir(), "missing.crt")
	check(proxy)

	CAFile = filepath.Join(t.TempDir(), "glov-egress-ca.crt")
	if err := os.WriteFile(CAFile, []byte("pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	withCA := maps.Clone(proxy)
	withCA["NODE_EXTRA_CA_CERTS"] = "/etc/ssl/certs/ca-certificates.crt"
	withCA["SSL_CERT_FILE"] = "/etc/ssl/certs/ca-certificates.crt"
	withCA["REQUESTS_CA_BUNDLE"] = "/etc/ssl/certs/ca-certificates.crt"
	check(withCA)
}
