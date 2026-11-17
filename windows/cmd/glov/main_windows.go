package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/service"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

// daemonVersion prints the running glovd's build and whether it speaks this glov's protocol.
func daemonVersion() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := dialGlovd(ctx, printProgress)
	if err != nil {
		fmt.Printf("glovd: not reachable (%v)\n", err)
		return nil
	}
	defer c.Close()
	var info api.Info
	if err := call(ctx, c, api.MethodInfo, nil, &info); err != nil {
		fmt.Printf("glovd: not reachable (%v)\n", err)
		return nil
	}
	verdict := "compatible"
	if info.ProtocolVersion != api.ProtocolVersion {
		verdict = "INCOMPATIBLE: upgrade glov or run host install"
	}
	fmt.Printf("glovd %s (protocol %d, %s)\n", info.Version, info.ProtocolVersion, verdict)
	return nil
}

func main() {
	cmd, err := parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "glov: %v\n\n%s", err, usage)
		os.Exit(exitUsage)
	}
	if err := run(cmd); err != nil {
		if code, ok := errors.AsType[exitCode](err); ok {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, "glov:", err)
		os.Exit(exitFailure)
	}
}

func run(cmd command) error {
	switch cmd.name {
	case "help":
		fmt.Print(usage)
		return nil
	case "version":
		fmt.Printf("glov %s (protocol %d)\n", version, api.ProtocolVersion)
		return daemonVersion()
	case "service install":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cache := cmd.cache
		if cache != "" {
			if cache, err = filepath.Abs(cache); err != nil {
				return err
			}
		}
		return service.Install(filepath.Dir(self), cache, os.Stdout)
	case "service uninstall":
		return service.Uninstall(os.Stdout)
	case "service install-broker":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		return service.InstallBroker(filepath.Dir(self), os.Stdout)
	case "service uninstall-broker":
		return service.UninstallBroker(os.Stdout)
	}

	if cmd.name == "guide" {
		fmt.Print(guide(cmd.topic))
		return nil
	}
	if cmd.name == "mcp" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return runMCP(ctx)
	}
	if cmd.name == "ssh-relay" {
		return runSSHRelay(context.Background(), cmd.vm)
	}
	if cmd.name == "share agent" {
		return runShareAgent(cmd.vm, cmd.share.Name)
	}

	// Long commands (a download, a guest that takes its time, a command
	// that runs for hours) end on Ctrl+C, which also ends their work in
	// glovd. Other commands answer at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if !longCommands[cmd.name] && !(cmd.name == "egress log" && cmd.watch) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := dialGlovd(dialCtx, printProgress)
	if err != nil {
		return err
	}
	defer c.Close()
	switch cmd.name {
	case "vm create":
		var vm api.VMInfo
		if err := call(ctx, c, api.MethodVMCreate, cmd.create, &vm); err != nil {
			return err
		}
		if len(vm.Harnesses) > 0 && !cmd.noSessions {
			if err := addSessionsShare(ctx, c, vm.Name); err != nil {
				return err
			}
		}
		// Provisioning powers the VM off; start it and wait for its Guest
		// Service, as macOS glov does, unless --no-start.
		if !cmd.noStart {
			defer ensureShareAgents(ctx, c, vm.Name)
			if err := call(ctx, c, api.MethodVMStart, api.VMParams{VM: cmd.vm}, &vm); err != nil {
				return err
			}
			if err := call(ctx, c, api.MethodVMWait, api.WaitParams{VM: cmd.vm, State: api.WaitGuestReady, Timeout: createWaitTimeout}, &vm); err != nil {
				return err
			}
		}
		fmt.Println(vm.Describe())
		if cmd.create.Open {
			fmt.Println("isolated: no network adapter; egress open: any public host through the egress proxy (vm configure --egress restricted for the allowlist)")
		}
		fmt.Print(describeHarnesses(ctx, c, vm.Name, vm.Harnesses))
	case "harness install":
		return runHarnessInstall(ctx, c, cmd.vm, cmd.harnesses, cmd.noHostConfig)
	case "harness add":
		return runHarnessAdd(ctx, c, cmd.vm, cmd.harnesses[0], cmd.noHostConfig)
	case "exec":
		return runExec(ctx, c, cmd.exec)
	case "cp":
		return runCp(ctx, c, cmd.src, cmd.dst)
	case "share add", "share remove", "share list":
		return runShare(ctx, c, cmd)
	case "share sync":
		return runShareSync(ctx, c, cmd.vm, cmd.ref, cmd.verify)
	case "share status":
		return runShareStatus(ctx, c, cmd.vm, cmd.share.Name, cmd.watch)
	case "agent":
		return runAgent(ctx, c, cmd)
	case "vm fetch":
		return runProjectFetch(ctx, c, cmd.vm)
	case "vm land":
		return runLand(ctx, c, cmd.vm, cmd.ref, cmd.yes)
	case "ssh":
		return runSSH(ctx, c, cmd.vm, cmd.sshExtra)
	case "egress list", "egress allow", "egress remove", "egress mcp", "egress log":
		return runEgress(ctx, c, cmd)
	case "secret set", "secret source", "secret refresh", "secret list", "secret remove", "secret grant", "secret revoke", "secret grants", "secret repos":
		return runSecret(ctx, c, cmd)
	case "secret login", "secret logout":
		return runSubscription(ctx, c, cmd)
	case "guest check":
		var res api.GuestCheck
		if err := call(ctx, c, api.MethodGuestCheck, api.VMParams{VM: cmd.vm}, &res); err != nil {
			return err
		}
		fmt.Println(cmd.vm+":", res.Describe())
	case "guest update":
		var res api.GuestUpdated
		if err := call(ctx, c, api.MethodGuestUpdate, api.GuestUpdateParams{VM: cmd.vm, Force: cmd.force, Timeout: cmd.timeout}, &res); err != nil {
			return err
		}
		if res.Updated {
			fmt.Printf("%s: Guest Service updated from %s to %s and answering\n", cmd.vm, res.From, res.To)
		} else {
			fmt.Printf("%s: Guest Service %s is already this build\n", cmd.vm, res.From)
		}
	case "stats":
		return runStats(ctx, c, cmd.vm, cmd.watch, cmd.json)
	case "vm configure":
		if cmd.egress != "" {
			var e api.Egress
			err := confirmingPlainSecrets(cmd.allowPlain, isConsole(os.Stdin), askPlainSecrets, func(allow bool) error {
				return c.Call(ctx, api.MethodEgressMode, api.EgressModeParams{VM: cmd.vm, Open: cmd.egress == "open", AllowPlainSecrets: allow}, &e)
			})
			if err != nil {
				return err
			}
			fmt.Println(describeEgressMode(e))
			return nil
		}
		var vm api.VMInfo
		p := api.ConfigureParams{VM: cmd.vm, CPUs: cmd.create.CPUs, MemoryMiB: cmd.create.MemoryMiB, DiskGiB: cmd.create.DiskGiB}
		if err := call(ctx, c, api.MethodVMConfigure, p, &vm); err != nil {
			return err
		}
		fmt.Println(vm.Describe())
	case "vm delete":
		repo, err := projectRemoteCheck(ctx, c, cmd.vm, cmd.yes)
		if err != nil {
			return err
		}
		if !cmd.yes && !confirm(fmt.Sprintf("Delete VM %s and its disk? [y/N] ", cmd.vm)) {
			return errors.New("not deleted")
		}
		records, _ := recordsDir(ctx, c, cmd.vm)
		var res api.VMDeleted
		if err := call(ctx, c, api.MethodVMDelete, api.VMParams{VM: cmd.vm}, &res); err != nil {
			return err
		}
		removeSSHFiles(cmd.vm)
		removeSSHHost(cmd.vm)
		if repo != nil {
			repo.forget(cmd.vm)
		}
		fmt.Println("deleted", cmd.vm)
		if line := forgetHerdrMachine(cmd.vm); line != "" {
			fmt.Println(line)
		}
		if records != "" {
			if _, err := os.Lstat(records); err == nil {
				fmt.Println(keptRecordsLine(records))
			}
		}
		for _, im := range res.PrunedImages {
			fmt.Println(im.Describe())
		}
	case "vm console":
		return printConsole(ctx, c, cmd.vm, cmd.watch)
	case "vm list":
		var vms []api.VMInfo
		if err := call(ctx, c, api.MethodVMList, nil, &vms); err != nil {
			return err
		}
		rows := make([]vmRow, len(vms))
		for i, v := range vms {
			rows[i] = vmRow{v.Name, v.Engine, v.State}
		}
		printList(os.Stdout, rows, "no VMs")
	case "vm status", "vm start", "vm stop", "vm reboot", "vm wait", "vm reset":
		method, params := map[string]string{
			"vm status": api.MethodVMStatus, "vm start": api.MethodVMStart, "vm stop": api.MethodVMStop, "vm reset": api.MethodVMReset,
			"vm reboot": api.MethodVMReboot, "vm wait": api.MethodVMWait,
		}[cmd.name], any(api.VMParams{VM: cmd.vm})
		switch cmd.name {
		case "vm stop":
			params = api.StopParams{VM: cmd.vm, Force: cmd.force}
			if !cmd.force {
				// A hung guest, which --force is for, would only make the fetch wait.
				fetchBeforeStop(ctx, c, cmd.vm)
			}
		case "vm reboot":
			params = api.RebootParams{VM: cmd.vm, Timeout: cmd.timeout}
		case "vm wait":
			params = api.WaitParams{VM: cmd.vm, State: cmd.state, Timeout: cmd.timeout}
		}
		var vm api.VMInfo
		if err := call(ctx, c, method, params, &vm); err != nil {
			return err
		}
		fmt.Println(vm.Describe())
		if cmd.name == "vm start" {
			ensureShareAgents(ctx, c, vm.Name)
		}
		if cmd.name == "vm status" && vm.Isolated {
			var e api.Egress
			if err := call(ctx, c, api.MethodEgressList, api.EgressParams{VM: vm.Name}, &e); err != nil {
				return err
			}
			fmt.Println(describeEgressLine(e))
			if p := vm.Project; p != nil {
				how := "through git"
				if p.Sync == api.SyncLive {
					how = "shared live"
				}
				fmt.Printf("  project: %s (%s)\n", p.Path, how)
			}
		}
		if cmd.name == "vm status" {
			if dir, ok := recordsDir(ctx, c, vm.Name); ok {
				bytes, files, exists := recordsUsage(dir)
				fmt.Println(describeRecords(dir, bytes, files, exists))
			}
		}
	case "trust list", "trust add", "trust remove":
		return runTrust(ctx, c, cmd)
	default:
		return fmt.Errorf("unhandled command %q", cmd.name)
	}
	return nil
}

// createWaitTimeout is how long vm create waits for a new VM's Guest
// Service, in seconds: the first boot after provisioning grows the disk.
const createWaitTimeout = 900

// longCommands run without glov's 30 s limit.
var longCommands = map[string]bool{"agent": true, "vm land": true, "vm create": true, "vm console": true, "vm stop": true, "vm reboot": true, "vm wait": true, "exec": true, "cp": true, "ssh": true, "share sync": true, "share status": true,
	"guest update": true, "stats": true, "harness install": true, "harness add": true,
	// A browser or device-code sign-in in the guest takes as long as the user does.
	"secret login": true, "secret logout": true}

// confirm asks question on stderr and reads a yes from stdin.
func confirm(question string) bool {
	fmt.Fprint(os.Stderr, question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// printProgress shows glovd's progress lines.
func printProgress(method string, params jsontext.Value) {
	var p api.Progress
	if method == api.NotifyProgress && json.Unmarshal(params, &p) == nil {
		fmt.Fprintln(os.Stderr, p.Message)
	}
}

// call shows a glovd error by its message alone, as macOS glov does.
func call(ctx context.Context, c caller, method string, params, result any) error {
	err := c.Call(ctx, method, params, result)
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		return errors.New(pe.Message)
	}
	return err
}

// printConsole prints vm's console log, and with follow keeps printing what
// the guest writes, across reboots, until Ctrl+C.
func printConsole(ctx context.Context, c caller, vm string, follow bool) error {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	text := &consoleText{w: out}
	p := api.ConsoleParams{VM: vm}
	for {
		var r api.ConsoleRead
		if err := call(ctx, c, api.MethodVMConsole, p, &r); err != nil {
			if follow && ctx.Err() != nil {
				return nil
			}
			return err
		}
		if r.Restarted {
			out.Flush()
			fmt.Fprintln(os.Stderr, "\n-- "+vm+" booted again --")
		}
		text.Write(r.Data)
		p.Log, p.Offset = r.Log, r.Offset
		if len(r.Data) > 0 {
			continue
		}
		if err := out.Flush(); err != nil || !follow {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}
