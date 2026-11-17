// Command glov-guest is the glov Guest Service. See docs/guest-protocol.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/akunzai/glov/guest/internal/guestservice"
	"github.com/akunzai/glov/guest/internal/logfile"
	"github.com/akunzai/glov/guest/internal/transport"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

const (
	retryEvery = 2 * time.Second
	logCap     = 1 << 20
)

func main() {
	stdio := flag.Bool("stdio", false, "serve the protocol over stdin/stdout instead of the guest port")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [--stdio] [--version] [install | uninstall]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println(version)
	case flag.Arg(0) == "install":
		exitOn(install())
	case flag.Arg(0) == "uninstall":
		exitOn(uninstall())
	case flag.NArg() > 0:
		flag.Usage()
		os.Exit(2)
	case !*stdio && runsAsService():
		exitOn(runService(func(ctx context.Context) { serve(ctx, false) }))
	default:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		serve(ctx, *stdio)
	}
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "glov-guest:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, stdio bool) {
	var out io.Writer = os.Stderr
	var logErr error
	if !stdio && logPath != "" {
		w, err := logfile.Open(logPath, logCap)
		if err == nil {
			defer w.Close()
			out = w
		}
		logErr = err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, nil)))
	if logErr != nil { // an unsafe or unusable log location: say so once, on stderr
		slog.Warn("log file disabled", "path", logPath, "err", logErr)
	}
	slog.Info("starting", "version", version, "stdio", stdio)

	a := guestservice.New(version)
	if stdio {
		first := true
		a.Run(ctx, func() (io.ReadWriteCloser, error) {
			if !first {
				return nil, io.EOF
			}
			first = false
			return transport.Stdio(), nil
		}, 0)
		return
	}
	serveGuest(ctx, a)
}
