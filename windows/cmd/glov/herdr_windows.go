package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/herdr"
	"github.com/akunzai/glov/windows/internal/project"
)

// herdrMachineWait is how long glov waits for hc to reach a machine, and
// retries herdr-app's space.new while herdr-app has not heard of a machine
// just added.
const herdrMachineWait = 60 * time.Second

// runHerdr is glov claude --herdr: the harness in herdr-core on machine vm,
// added once the owner agrees, then shown in herdr-app's space for the
// project (guest, its path in the guest), or attached here. attached
// reports whether this terminal was attached until it detached or the
// harness ended. hc reaches the machine through glov ssh <vm> --, which
// sets SSH up in the guest each time it connects.
func runHerdr(ctx context.Context, cmd command, vm, root, guest string, prof harness.Profile) (attached bool, err error) {
	hc, err := exec.LookPath("hc")
	if err != nil {
		return false, errors.New("--herdr needs herdr-core's hc on PATH (https://herdr.dev)")
	}
	fmt.Fprintln(os.Stderr, herdr.Warning)
	out, err := herdrTool(hc, "machine", "ls", "--json")
	if err != nil {
		return false, err
	}
	machines := herdr.Machines(out)
	if !slices.ContainsFunc(machines, func(m herdr.Machine) bool { return herdr.IsVM(m, vm) }) {
		if slices.ContainsFunc(machines, func(m herdr.Machine) bool { return m.Name == vm }) {
			return false, fmt.Errorf("herdr-core already has a machine named %s that is not reached through glov ssh; hc machine rm %s first", vm, vm)
		}
		if !cmd.agent.yes {
			if !isConsole(os.Stdin) {
				return false, fmt.Errorf("adding %s as a herdr-core machine needs your yes: run in a terminal, or with --yes", vm)
			}
			if a := strings.ToLower(strings.TrimSpace(ask(fmt.Sprintf("add %s as a herdr-core machine, with the risks above? [y/N] ", vm)))); a != "y" && a != "yes" {
				return false, fmt.Errorf("not added; glov %s without --herdr uses tmux", prof.Command)
			}
		}
		glov, err := os.Executable()
		if err != nil {
			return false, err
		}
		fmt.Fprintf(os.Stderr, "adding %s as a herdr-core machine (hc is copied into the VM)\n", vm)
		if _, err := herdrTool(hc, "machine", "add", "--via", herdr.Via(glov, vm), "--name", vm, "--install"); err != nil {
			if strings.Contains(err.Error(), "run by sh here") {
				// Before 0.1.20, Windows' hc refused every --via.
				return false, errors.New("--herdr needs hc 0.1.20 or later: hc update")
			}
			return false, err
		}
	}
	if err := waitForMachine(ctx, hc, vm); err != nil {
		return false, err
	}

	argv := append([]string{prof.Command}, cmd.agent.args...)
	if os.Getenv("HC_TERMINAL_ID") != "" {
		if app, err := exec.LookPath("herdr-app"); err == nil {
			if ls, err := herdrTool(app, "ls"); err == nil {
				return false, herdrAppSession(ctx, hc, app, ls, cmd, vm, root, guest, prof, argv)
			}
		}
	}
	out, err = herdrTool(hc, "ls", "--json")
	if err != nil {
		return false, err
	}
	existing := herdr.LiveNames(out, vm)
	session := project.Session(prof.Name, existing, cmd.agent.fresh)
	running := slices.Contains(existing, session)
	if running {
		switch now := occupant(ctx, hc, vm+"/"+session, prof); {
		case now.Shell:
			fmt.Fprintln(os.Stderr, herdr.Restarting(session, vm, prof.Command))
			if _, err := herdrTool(hc, "kill", vm+"/"+session); err != nil {
				return false, err
			}
			herdrTool(hc, "rm", vm+"/"+session)
			running = false
		case now.Other != "":
			fmt.Fprintln(os.Stderr, herdr.LeftAlone(session, vm, now.Other, prof.Command))
		}
	}
	if running {
		fmt.Fprintf(os.Stderr, "attaching %s/%s (detach with ctrl+\\)\n", vm, session)
	} else {
		fmt.Fprintf(os.Stderr, "starting %s/%s (detach with ctrl+\\)\n", vm, session)
		// No parent: a terminal in the VM may only reach the Windows
		// terminals that started its own.
		if _, err := herdrTool(hc, append([]string{"run", "--on", vm, "--no-parent", "--keep", "--bg", "--name", session, "--cwd", guest, "--"}, argv...)...); err != nil {
			return false, err
		}
	}
	attach := exec.Command(hc, "attach", vm+"/"+session)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl+C belongs to the harness now.
	signal.Ignore(os.Interrupt)
	err = attach.Run()
	if e, ok := errors.AsType[*exec.ExitError](err); ok && e.ExitCode() != 0 {
		return true, exitCode(e.ExitCode())
	}
	return true, err
}

// herdrAppSession shows the harness in herdr-app (whose ls is ls): the
// running session in the project's space of machine vm, or a new tab
// there, or a new space.
func herdrAppSession(ctx context.Context, hc, app string, ls []byte, cmd command, vm, root, guest string, prof harness.Profile, argv []string) error {
	found := herdr.PlaceOf(ls, vm, guest, prof.Name)
	session := project.Session(prof.Name, found.Tabs, cmd.agent.fresh)
	stale := ""
	if !cmd.agent.fresh && found.Terminal != "" {
		now := occupant(ctx, hc, found.Terminal, prof)
		if !now.Shell {
			if now.Other != "" {
				fmt.Fprintln(os.Stderr, herdr.LeftAlone(session, vm, now.Other, prof.Command))
			} else {
				fmt.Fprintf(os.Stderr, "showing the running session %s in herdr\n", session)
			}
			return herdrDo(app, "agent.focus", map[string]any{"terminal": found.Terminal})
		}
		fmt.Fprintln(os.Stderr, herdr.Restarting(session, vm, prof.Command))
		stale = found.Tab
	}
	if found.Space != "" {
		fmt.Fprintf(os.Stderr, "starting %s in herdr's space for %s\n", session, vm)
		if err := herdrDo(app, "tab.new", map[string]any{"space": found.Space, "name": session, "folder": guest, "argv": argv}); err != nil || stale == "" {
			return err
		}
		// After the new tab: closing a space's last tab closes the space.
		return herdrDo(app, "tab.close", map[string]any{"id": stale, "confirmed": true})
	}
	fmt.Fprintf(os.Stderr, "starting %s in a new herdr space for %s\n", session, vm)
	request := map[string]any{"name": filepath.Base(root) + " (vm)", "folder": guest, "machine": vm, "tab": session, "argv": argv}
	// herdr-app hears of a machine just added a little later.
	deadline := time.Now().Add(herdrMachineWait)
	for {
		err := herdrDo(app, "space.new", request)
		if err == nil || !strings.Contains(err.Error(), "no machine called") || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// herdrRestoreWait is how long glov waits for hc to give a terminal lost
// to a VM restart its new shell (about 2 s after the machine connects).
const herdrRestoreWait = 15 * time.Second

// occupant is what terminal runs now (herdr.OccupantOf), once hc has
// restored it after a VM restart; the harness when hc cannot say.
func occupant(ctx context.Context, hc, terminal string, prof harness.Profile) herdr.Occupant {
	deadline := time.Now().Add(herdrRestoreWait)
	for {
		out, err := herdrTool(hc, "get", terminal, "--json")
		if err != nil {
			return herdr.Occupant{}
		}
		if !herdr.Restoring(out) || time.Now().After(deadline) {
			return herdr.OccupantOf(out, prof.Command)
		}
		select {
		case <-ctx.Done():
			return herdr.Occupant{}
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitForMachine waits until hc reaches machine vm: after the VM starts,
// hc keeps answering with the last failure until its next retry.
func waitForMachine(ctx context.Context, hc, vm string) error {
	deadline := time.Now().Add(herdrMachineWait)
	for said := false; ; said = true {
		out, err := herdrTool(hc, "machine", "ls", "--json")
		if err != nil {
			return err
		}
		i := slices.IndexFunc(herdr.Machines(out), func(m herdr.Machine) bool { return m.Name == vm })
		if i < 0 {
			return fmt.Errorf("herdr-core has no machine %s any more", vm)
		}
		m := herdr.Machines(out)[i]
		if m.State == "connected" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("herdr-core cannot reach %s (%s, %s); hc machine ls shows when it can", vm, m.State, m.Why)
		}
		if !said {
			fmt.Fprintf(os.Stderr, "waiting for herdr-core to reach %s (up to %d s)\n", vm, int(herdrMachineWait.Seconds()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// herdrDo runs herdr-app do command with args as one JSON object.
func herdrDo(app, command string, args map[string]any) error {
	b, err := json.Marshal(args, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = herdrTool(app, "do", command, string(b))
	return err
}

// forgetHerdrMachine removes VM vm from herdr-core's machines when it was
// added for it; the line to print, if any.
func forgetHerdrMachine(vm string) string {
	hc, err := exec.LookPath("hc")
	if err != nil {
		return ""
	}
	out, err := herdrTool(hc, "machine", "ls", "--json")
	if err != nil || !slices.ContainsFunc(herdr.Machines(out), func(m herdr.Machine) bool { return herdr.IsVM(m, vm) }) {
		return ""
	}
	if _, err := herdrTool(hc, "machine", "rm", vm); err != nil {
		return fmt.Sprintf("warning: herdr-core machine %s is still there (%v); hc machine rm %s", vm, err, vm)
	}
	return "removed herdr-core machine " + vm
}

// herdrTool runs hc or herdr-app (path) with args and returns its output;
// it fails with its stderr.
func herdrTool(path string, args ...string) ([]byte, error) {
	cmd := exec.Command(path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		return nil, fmt.Errorf("%s %s failed: %s", name, strings.Join(args[:min(2, len(args))], " "), detail)
	}
	return stdout.Bytes(), nil
}
