package exec

import (
	"os"
	osexec "os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// configure keeps the child off any console window; its stdio is piped.
func configure(cmd *osexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

func exitCode(ps *os.ProcessState) int { return ps.ExitCode() }

// attach puts the started child into a job object so Kill takes down its whole
// tree. shortcut: the assignment happens after CreateProcess, so a child that
// forks within microseconds of starting escapes the job; upgrade to
// CREATE_SUSPENDED plus ResumeThread if that ever shows up.
func attach(cmd *osexec.Cmd) (kill func() error, release func()) {
	job, err := newJobFor(uint32(cmd.Process.Pid))
	if err != nil {
		return cmd.Process.Kill, func() {}
	}
	return func() error { return windows.TerminateJobObject(job, 1) },
		func() { windows.CloseHandle(job) }
}

func newJobFor(pid uint32) (windows.Handle, error) {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	return assignToNewJob(h)
}

func assignToNewJob(process windows.Handle) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}
