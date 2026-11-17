package exec

import (
	"io"
	"os"
	osexec "os/exec"
	"runtime"
)

// sysChild is a process started with os/exec in the Guest Service's own account.
type sysChild struct {
	cmd     *osexec.Cmd
	stdout  io.Reader
	stderr  io.Reader
	kill    func() error
	release func()
}

func (c *sysChild) Pid() int          { return c.cmd.Process.Pid }
func (c *sysChild) Stdout() io.Reader { return c.stdout }
func (c *sysChild) Stderr() io.Reader { return c.stderr }
func (c *sysChild) Kill() error       { return c.kill() }
func (c *sysChild) Close()            { c.release() }

func (c *sysChild) Wait() int {
	c.cmd.Wait() // an error here is just a non-zero exit, carried by ExitCode
	return exitCode(c.cmd.ProcessState)
}

func startSystem(p StartParams) (child, io.WriteCloser, error) {
	cmd := osexec.Command(p.Argv[0], p.Argv[1:]...)
	cmd.Dir = p.Cwd
	if len(p.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), p.Env, runtime.GOOS == "windows")
	}
	return launch(cmd, p.Stdin)
}

// launch starts cmd with piped stdio. The caller has set Path, Args, Dir, Env
// and, for another account, SysProcAttr.Credential.
func launch(cmd *osexec.Cmd, withStdin bool) (child, io.WriteCloser, error) {
	var in io.WriteCloser
	if withStdin {
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
		in = w
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	configure(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	kill, release := attach(cmd)
	return &sysChild{cmd, stdout, stderr, kill, release}, in, nil
}
