// glov-broker is glov's one LocalSystem service: it makes the Host
// Compute Service calls each user's glovd cannot make without elevation
// (see package broker). `glov host install-broker` installs it, once, from
// an elevated shell.
//
//	glov-broker [--data DIR]
//
// Run outside the service manager (elevated, for development) it serves in
// the foreground until Ctrl+C.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/service"
	"github.com/akunzai/glov/windows/internal/winsec"
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("glov-broker", flag.ExitOnError)
	data := fs.String("data", service.BrokerDataDir(), "data directory")
	fs.Parse(os.Args[1:])
	isService, err := svc.IsWindowsService()
	if err != nil {
		fatal(err)
	}
	r := &runner{data: *data, s: &broker.Server{
		Owners: &broker.Owners{Path: filepath.Join(*data, "owners.json")}, HCS: broker.HCSSystems{}, Version: version,
		SignedIn: broker.SignedIn,
	}}
	if isService {
		if err := svc.Run(service.BrokerName, r); err != nil {
			fatal(err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := r.run(ctx, func() {}); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "glov-broker:", err)
	os.Exit(1)
}

type runner struct {
	data string
	s    *broker.Server
}

// run checks the data directory, opens the pipe, calls ready and serves
// until ctx ends.
func (r *runner) run(ctx context.Context, ready func()) error {
	pin, err := winsec.PinDir(r.data)
	if err != nil {
		return err
	}
	defer pin.Close()
	if err := winsec.CheckDataDir(r.data, ""); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(r.data, "glov-broker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	// A service has no console: a crash would otherwise leave no trace.
	if err := debug.SetCrashOutput(logf, debug.CrashOptions{}); err != nil {
		slog.Warn("crash output", "err", err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(logf, os.Stderr), nil)))
	ln, err := broker.Listen()
	if err != nil {
		return fmt.Errorf("listen on %s: %w", broker.PipeName, err)
	}
	slog.Info("glov-broker started", "version", version, "protocol", broker.Protocol, "data", r.data)
	ready()
	err = r.s.Serve(ctx, ln)
	slog.Info("glov-broker stopped")
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Execute runs the broker under the service manager. Stopping the service
// (an upgrade) leaves the VMs running. The host shutting down stops them
// first, as HCS would otherwise turn them off before anyone hears of it,
// and a user signing out of their last session stops theirs.
func (r *runner) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	const accepts = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown | svc.AcceptSessionChange
	go func() {
		done <- r.run(ctx, func() { status <- svc.Status{State: svc.Running, Accepts: accepts} })
	}()
	// preShutdown is set once the host began shutting down; stopping is
	// closed once the VMs have stopped, and nil again after.
	var preShutdown bool
	var stopping chan struct{}
	var checkpoint uint32
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			cancel()
			if err != nil {
				// Report why it stopped in the Application event log, where
				// glov host install-broker registered the source.
				if el, eerr := eventlog.Open(service.BrokerName); eerr == nil {
					el.Error(1, err.Error())
					el.Close()
				}
				return true, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.SessionChange:
				if c.EventType == windows.WTS_SESSION_LOGOFF && c.EventData != 0 {
					// EventData points at a WTSSESSION_NOTIFICATION the
					// service manager owns for the call.
					session := (*windows.WTSSESSION_NOTIFICATION)(unsafe.Add(nil, c.EventData)).SessionID
					go r.s.SignedOut(ctx, session)
				}
			case svc.PreShutdown:
				if preShutdown {
					break
				}
				preShutdown = true
				stopping = make(chan struct{})
				checkpoint++
				status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: stopHint}
				go func(stopping chan struct{}) {
					// Past this, HCS itself is not answering; let the host go on.
					sctx, scancel := context.WithTimeout(ctx, broker.DefaultStopTimeout+20*time.Second)
					defer scancel()
					slog.Info("stopping every VM for host shutdown")
					r.s.Stop(sctx, "")
					close(stopping)
				}(stopping)
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
			}
		case <-tick.C:
			// Tell the service manager the VMs are still stopping.
			if stopping != nil && ctx.Err() == nil {
				checkpoint++
				status <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: stopHint}
			}
		case <-stopping:
			stopping = nil
			cancel()
		}
	}
}

// stopHint is the wait hint while the VMs stop at host shutdown.
const stopHint = 10 * 1000 // ms
