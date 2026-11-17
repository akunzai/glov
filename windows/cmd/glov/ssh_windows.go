package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// sshFilesFor is where glov keeps vm's SSH client files:
// %LOCALAPPDATA%\glov\vms\<vm>.
func sshFilesFor(vm string) (sshFiles, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return sshFiles{}, err
	}
	return sshFiles{filepath.Join(base, "glov", "vms", vm)}, nil
}

// openSSHTool finds one of Windows' OpenSSH client programs (ssh,
// ssh-keygen): the optional feature's copy first, then PATH.
func openSSHTool(name string) (string, error) {
	p := filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", name+".exe")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s not found: add Windows' OpenSSH Client (Settings > System > Optional features)", name)
	}
	return p, nil
}

// runSSH signs in to vm's guest as sshUser with ssh, or runs a command
// there, and exits with ssh's exit code.
func runSSH(ctx context.Context, c *rpc.Client, vm string, extra []string) error {
	var info api.VMInfo
	if err := call(ctx, c, api.MethodVMStatus, api.VMParams{VM: vm}, &info); err != nil {
		return err
	}
	if info.State != "running" {
		return fmt.Errorf("VM %s is %s: glov vm start %s", info.Name, info.State, info.Name)
	}
	ssh, err := openSSHTool("ssh")
	if err != nil {
		return err
	}
	files, err := sshFilesFor(info.Name)
	if err != nil {
		return err
	}
	pub, err := ensureKey(files, info.Name)
	if err != nil {
		return err
	}
	// The guest may be new, or newer than the key: set it up every time.
	code, out, err := guestRun(ctx, c, info.Name, []string{"/bin/sh", "-c", sshSetup, "sh", pub})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("setting up SSH in the guest exited %d: %s", code, strings.TrimSpace(string(out)))
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(ssh, sshArgs(info.Name, files, sshRelayCommand(self, info.Name), info.Isolated, extra)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl+C belongs to ssh and the remote command now.
	signal.Ignore(os.Interrupt)
	err = cmd.Run()
	if e, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitCode(e.ExitCode())
	}
	return err
}

// ensureKey creates files' key pair with ssh-keygen unless it exists, and
// returns the public key's line.
func ensureKey(files sshFiles, vm string) (string, error) {
	if _, err := os.Stat(files.key()); errors.Is(err, os.ErrNotExist) {
		keygen, err := openSSHTool("ssh-keygen")
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(files.dir, 0o700); err != nil {
			return "", err
		}
		os.Remove(files.key() + ".pub")
		out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "glov@"+vm, "-f", files.key()).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("ssh-keygen: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// The key lives in the glov home, whose every file glovd checks at
	// start; ssh-keygen's own ACLs would stop it.
	me, err := winsec.CurrentSID()
	if err != nil {
		return "", err
	}
	for _, f := range []string{files.key(), files.key() + ".pub"} {
		if err := winsec.ProtectFile(f, me); err != nil {
			return "", err
		}
	}
	pub, err := os.ReadFile(files.key() + ".pub")
	if err != nil {
		return "", fmt.Errorf("the SSH key's public half: %w (delete %s to make a new key)", err, files.dir)
	}
	return strings.TrimSpace(string(pub)), nil
}

// removeSSHFiles drops a deleted VM's SSH client files, so a new VM of the
// same name gets a new key and is not taken for a changed host, and its
// live shares' state. A live share's agent may hold its log open for a
// few seconds more, until it sees the VM gone (liveVMPoll).
func removeSSHFiles(vm string) {
	files, err := sshFilesFor(vm)
	if err != nil {
		return
	}
	for deadline := time.Now().Add(3 * liveVMPoll); os.RemoveAll(files.dir) != nil && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
	}
}

// runSSHRelay is ssh's ProxyCommand: it pipes stdin and stdout to vm's sshd
// through glovd.
func runSSHRelay(ctx context.Context, vm string) error {
	conn, err := dialGlovdSSH(ctx, vm)
	if err != nil {
		return err
	}
	defer conn.Close()
	var once sync.Once
	done := make(chan struct{})
	end := func() { once.Do(func() { close(done) }) }
	go func() { io.Copy(conn, os.Stdin); end() }()
	go func() { io.Copy(os.Stdout, conn); end() }()
	<-done
	return nil
}
