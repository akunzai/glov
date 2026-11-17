package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/hcs"
	"github.com/akunzai/glov/windows/internal/progress"
	"github.com/akunzai/glov/windows/internal/seed"
)

// Broker is what a provisioning boot asks of glov-broker; *broker.Client
// is the real one. The VM is started without KeepRunning, so the broker
// turns it off should the connection end first.
type Broker interface {
	Create(ctx context.Context, p broker.CreateParams) error
	Start(ctx context.Context, id string) error
	State(ctx context.Context, id string) (string, error)
	Terminate(ctx context.Context, id string) error
}

// SeedFile is the seed disk inside the VM's directory while it provisions.
const SeedFile = "seed.vhd"

// Timeout bounds a whole provisioning boot. It takes seconds; a VM that
// has not reported by then is not going to.
const Timeout = 5 * time.Minute

// consoleTail is how many console lines a failure logs.
const consoleTail = 40

// Run boots vm once with a seed disk holding guest, the Guest Service's Linux
// binary, and returns once the guest has reported and powered off. report
// receives each step, and a still line when the guest has reported nothing
// for a while. On failure the console's last lines go to slog.
func Run(ctx context.Context, b Broker, vm VM, guest []byte, report func(string)) error {
	seedPath := filepath.Join(vm.Dir, SeedFile)
	if err := seed.Write(seedPath, Files(vm.ID, vm.Name, vm.Isolated, vm.Harnesses, vm.EgressCA, guest)); err != nil {
		return fmt.Errorf("seed disk: %w", err)
	}
	defer os.Remove(seedPath)
	// The VM's worker process opens its disks as the VM's own account.
	for _, p := range []string{vm.Disk, vm.BaseDisk, seedPath} {
		if err := hcs.GrantVmAccess(vm.ID, p); err != nil {
			return fmt.Errorf("grant the VM %s: %w", p, err)
		}
	}
	ln, err := broker.ListenConsole(vm.ID)
	if err != nil {
		return fmt.Errorf("console pipe: %w", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	tail := make([]string, 0, consoleTail)
	// Until the first marker the kernel boots; a slow step (a harness
	// install) prints nothing between markers either.
	steps := progress.Start("booting the VM", report, progress.Every)
	defer steps.Stop()
	result := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			result <- fmt.Errorf("console: %w", err)
			return
		}
		defer c.Close()
		// Closing the pipe ends Watch when ctx ends first.
		stop := context.AfterFunc(ctx, func() { c.Close() })
		defer stop()
		result <- Watch(c, steps.Step, func(line string) {
			if len(tail) == consoleTail {
				tail = tail[1:]
			}
			tail = append(tail, line)
		})
	}()

	if err := b.Create(ctx, broker.CreateParams{ID: vm.ID, CPUs: vm.CPUs, MemoryMiB: vm.MemoryMiB,
		Disk: vm.Disk, Seed: seedPath, SecureBoot: api.SecureBoot(vm.Distro)}); err != nil {
		return fmt.Errorf("create the VM: %w", err)
	}
	// Turned off on every way out; a VM that powered off is gone already.
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		b.Terminate(tctx, vm.ID)
	}()
	if err := b.Start(ctx, vm.ID); err != nil {
		return fmt.Errorf("start the VM: %w", err)
	}
	select {
	case err = <-result:
	case <-ctx.Done():
		ln.Close()
		err = <-result
	}
	if err == nil {
		steps.Step("waiting for the VM to power off")
		waitStopped(ctx, b, vm.ID)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("no report from the VM within %s: %w", Timeout, ErrNoReport)
		} else if ctx.Err() != nil {
			err = ctx.Err()
		}
		slog.Error("provisioning failed", "vm", vm.Name, "err", err, "console", strings.Join(tail, "\n"))
	}
	return err
}

// waitStopped gives a guest that reported Done time to power off on its own,
// so its file systems are clean; the caller terminates it after.
func waitStopped(ctx context.Context, b Broker, id string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if state, err := b.State(ctx, id); err != nil || state != broker.Running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}
