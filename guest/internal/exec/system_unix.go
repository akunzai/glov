//go:build !windows

package exec

import (
	"os"
	osexec "os/exec"
	"syscall"
)

// configure puts the child in its own process group so Kill reaches its tree.
func configure(cmd *osexec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// exitCode follows the shell convention for a process ended by a signal
// (exec.kill sends SIGKILL, so 137); os.ProcessState reports -1 for it.
func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func attach(cmd *osexec.Cmd) (kill func() error, release func()) {
	return func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }, func() {}
}
