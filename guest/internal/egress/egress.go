// Package egress bridges an isolated guest's HTTP proxy clients to the host's
// egress proxy (docs/guest-protocol.md "Isolated VM egress proxy"). It parses
// nothing and decides nothing: the host enforces the allowlist.
package egress

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"

	"github.com/akunzai/glov/guest/internal/transport"
)

const (
	// Addr is where guest programs find the proxy.
	Addr = "127.0.0.1:3128"
	// HostPort is the host's vsock port for egress connections.
	HostPort = 4024
	// Marker is the root-owned file first boot writes in an isolated VM.
	Marker = "/etc/glov/egress-proxy"
	// Identity is the marker's exact content.
	Identity = "glov-egress-v1\n"
	maxConns = 256
)

// CAFile is where first boot writes the egress CA; a variable for tests.
var CAFile = "/usr/local/share/ca-certificates/glov-egress-ca.crt"

// Env is the proxy environment of guest programs. When the egress CA is
// installed (a Mac host, which uses it for hosts a granted secret is bound
// to), it adds the variables that point programs skipping the system trust
// store at the system bundle, which holds it and any glov vm trust
// certificate, mirroring the host's CloudInitSeed.trustEnvironment. Without
// the file they are left out, so Node does not warn about a missing CA.
func Env() map[string]string {
	url, none := "http://"+Addr, "localhost,127.0.0.1,::1"
	env := map[string]string{
		"HTTP_PROXY": url, "HTTPS_PROXY": url, "http_proxy": url, "https_proxy": url,
		"NO_PROXY": none, "no_proxy": none,
	}
	if _, err := os.Stat(CAFile); err == nil {
		bundle := "/etc/ssl/certs/ca-certificates.crt"
		env["NODE_EXTRA_CA_CERTS"] = bundle
		env["SSL_CERT_FILE"] = bundle
		env["REQUESTS_CA_BUNDLE"] = bundle
	}
	return env
}

// Serve accepts clients on l until ctx ends, piping each to a new connection
// from dial. Past maxConns clients at once, a new one is closed.
func Serve(ctx context.Context, l net.Listener, dial func() (io.ReadWriteCloser, error)) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	defer l.Close()
	slots := make(chan struct{}, maxConns)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); bridge(c, dial) }()
		default:
			c.Close()
		}
	}
}

func bridge(c net.Conn, dial func() (io.ReadWriteCloser, error)) {
	defer c.Close()
	host, err := dial()
	if err != nil {
		slog.Warn("egress proxy unreachable", "err", err)
		return
	}
	defer host.Close()
	transport.Pipe(c, host)
}
