// glovd is glov's per-user daemon on Windows: it runs as the user who
// signed in (Windows starts it at sign-in; glov host install sets that up),
// keeps their VMs, secrets and egress proxies in their glov home, and has
// glov-broker make the HCS calls a user cannot.
//
//	glovd [--data DIR] [--cache DIR] [--pipe PATH]
//
// It is built as a GUI program, so starting it opens no console window.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/baseimage"
	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/daemon"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/provision"
	"github.com/akunzai/glov/windows/internal/secret"
	"github.com/akunzai/glov/windows/internal/service"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/vhdx"
	"github.com/akunzai/glov/windows/internal/vmrun"
	"github.com/akunzai/glov/windows/internal/winsec"
)

var version = "dev"

func main() {
	me, err := winsec.CurrentSID()
	if err != nil {
		fatal(err)
	}
	fs := flag.NewFlagSet("glovd", flag.ExitOnError)
	data := fs.String("data", service.Home(), "data directory (the glov home)")
	cache := fs.String("cache", "", "image cache directory (default: the data directory's cache folder)")
	pipe := fs.String("pipe", api.Pipe(me), "control pipe")
	fs.Parse(os.Args[1:])
	if *cache == "" {
		*cache = filepath.Join(*data, "cache")
	}
	if winsec.Elevated() {
		fatal(errors.New("glovd must not run elevated: the files it made would belong to Administrators, which glov-broker refuses to boot"))
	}
	images := &baseimage.Cache{Dir: *cache, Client: http.DefaultClient}
	host := &broker.Conn{}
	d := &daemon.Daemon{
		Owner: me, Store: store.Store{Root: *data}, Cache: *cache, Version: version,
		Prepare: func(ctx context.Context, distro string, progress func(string)) (daemon.Image, error) {
			b, err := images.Prepare(ctx, distro, progress)
			return daemon.Image{Release: b.Release, BaseDisk: b.Disk()}, err
		},
		NewDisk: vhdx.Differencing,
		Provision: func(ctx context.Context, vm provision.VM, progress func(string)) error {
			guest, err := readGuest()
			if err != nil {
				return err
			}
			// A connection of its own: should glovd go away mid-boot, the
			// broker turns the half-provisioned VM off.
			b, err := broker.Dial(ctx)
			if err != nil {
				return err
			}
			defer b.Close()
			return provision.Run(ctx, b, vm, guest, progress)
		},
		PruneImages: func(inUse func(string) bool) ([]api.PrunedImage, bool, error) {
			pruned, busy, err := images.Prune(inUse)
			out := make([]api.PrunedImage, len(pruned))
			for i, p := range pruned {
				out[i] = api.PrunedImage{Release: p.Release, Bytes: p.Bytes}
			}
			return out, busy, err
		},
		ResizeDisk:  vhdx.Resize,
		MergeDisk:   vhdx.Merge,
		VMs:         vmrun.NewManager(host),
		GuestBinary: readGuest,
		HostMemory:  vmrun.HostMemory,
		Secrets:     &secret.Store{Dir: filepath.Join(*data, "secrets"), Values: &secret.Files{Dir: filepath.Join(*data, "secrets")}},
		CADir:       filepath.Join(*data, egress.CADir),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, quit := context.WithCancel(ctx)
	d.Quit = quit
	if err := run(ctx, d, me, *data, *cache, *pipe); err != nil {
		fatal(err)
	}
}

// readGuest reads the Guest Service binary that glov host install copied
// next to glovd.exe.
func readGuest() ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	guest, err := os.ReadFile(filepath.Join(filepath.Dir(exe), service.GuestBinary))
	if err != nil {
		return nil, fmt.Errorf("the Guest Service binary: %w (reinstall with glov host install)", err)
	}
	return guest, nil
}

// fatal reports err in the log, as a GUI program has no console, and on
// stderr for a glovd started from a shell.
func fatal(err error) {
	slog.Error("glovd stopped", "err", err)
	fmt.Fprintln(os.Stderr, "glovd:", err)
	os.Exit(1)
}

// run checks the data and cache directories, opens the control pipe and
// serves until ctx ends. Both directories stay pinned while it runs, so
// the paths it checked cannot be swapped for others.
func run(ctx context.Context, d *daemon.Daemon, me, data, cache, pipe string) error {
	for _, dir := range []string{data, cache} {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			if err := winsec.CreateDataDir(dir, me); err != nil {
				return err
			}
		}
		pin, err := winsec.PinDir(dir)
		if err != nil {
			return err
		}
		defer pin.Close()
		if err := winsec.CheckDataDir(dir, me); err != nil {
			return err
		}
	}
	logf, err := os.OpenFile(filepath.Join(data, "glovd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	// Without a console a crash would otherwise leave no trace.
	if err := debug.SetCrashOutput(logf, debug.CrashOptions{}); err != nil {
		slog.Warn("crash output", "err", err)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter{logf}, nil)))
	ln, err := d.Listen(pipe, me)
	if err != nil {
		return fmt.Errorf("listen on %s (is glovd running already?): %w", pipe, err)
	}
	// VMs outlive glovd; track the ones still running.
	d.Adopt()
	slog.Info("glovd started", "version", version, "owner", d.Owner, "data", data, "cache", cache)
	err = d.Serve(ctx, ln)
	slog.Info("glovd stopped")
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// logWriter writes the log to its file and, when glovd has a console, to
// stderr too.
type logWriter struct{ f *os.File }

func (w logWriter) Write(p []byte) (int, error) {
	os.Stderr.Write(p) // fails harmlessly without a console
	return w.f.Write(p)
}
