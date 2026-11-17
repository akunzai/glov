package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/jobrun"
	"github.com/akunzai/glov/windows/internal/rpc"
)

// exitNoUser is glov exec's exit status for --as user while nobody is
// signed in (Guest Service error 4): sysexits' EX_TEMPFAIL, as on macOS.
const exitNoUser = 75

// exitCode makes glov exit with a status and no message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// runExec runs cmd.exec in the guest, streams its output to stdout and
// returns its exit status as an exitCode. Ctrl+C (ctx ending) kills it.
func runExec(ctx context.Context, c *rpc.Client, p api.ExecParams) error {
	var job api.JobStarted
	if err := c.Call(ctx, api.MethodExecStart, p, &job); err != nil {
		if pe, ok := errors.AsType[*proto.Error](err); ok {
			fmt.Fprintln(os.Stderr, "glov:", pe.Message)
			if pe.Code == proto.CodeNoUser {
				return exitCode(exitNoUser)
			}
			return exitCode(exitFailure)
		}
		return err
	}
	code, err := follow(ctx, c, p.VM, job.JobID, os.Stdout)
	if ctx.Err() != nil {
		return exitCode(130)
	}
	if err != nil {
		return err
	}
	// Windows keeps 32 bits, but keep the guest's 8 and never turn a
	// failure into 0.
	if code != 0 && code&0xFF == 0 {
		return exitCode(1)
	}
	return exitCode(code & 0xFF)
}

// follow copies a job's output to w until it exits and returns its code.
// Ending ctx kills the job.
func follow(ctx context.Context, c caller, vm, job string, w io.Writer) (int, error) {
	res, err := jobrun.Run(ctx, rpcJob{c, vm, job}, w, 0)
	if res.Dropped {
		fmt.Fprintln(os.Stderr, "glov: output was too fast to follow; some of it was skipped")
	}
	if err != nil {
		return 0, err
	}
	if res.State == jobrun.Lost {
		return 0, errors.New("lost track of the command: the VM stopped or rebooted, or its Guest Service was replaced")
	}
	return res.Code, nil
}

// rpcJob is a job started through glovd's exec.start.
type rpcJob struct {
	c  caller
	vm string
	id string
}

func (j rpcJob) Read(ctx context.Context, cursor int) (jobrun.Read, error) {
	var r api.ExecRead
	err := call(ctx, j.c, api.MethodExecRead, api.ExecReadParams{VM: j.vm, JobID: j.id, Cursor: cursor}, &r)
	return jobrun.Read{Data: r.Data, Cursor: r.Cursor, Exited: r.Exited, Code: r.Code, Lost: r.Lost, Dropped: r.Dropped}, err
}

func (j rpcJob) Kill(ctx context.Context) error {
	return j.c.Call(ctx, api.MethodExecKill, api.ExecKillParams{VM: j.vm, JobID: j.id}, nil)
}

// daemonGuest is a VM's file system through glovd's guest.call.
type daemonGuest struct {
	ctx context.Context
	c   caller
	vm  string
}

func (g daemonGuest) call(method string, params, result any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var res jsontext.Value
	if err := call(g.ctx, g.c, api.MethodGuestCall, api.GuestCallParams{VM: g.vm, Method: method, Params: b}, &res); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(res, result)
}

// mkdirs runs mkdir -p in the guest, a few hundred directories at a time.
func (g daemonGuest) mkdirs(dirs []string) error {
	for len(dirs) > 0 {
		n := min(len(dirs), 200)
		argv := append([]string{"mkdir", "-p", "--"}, dirs[:n]...)
		dirs = dirs[n:]
		code, out, err := guestRun(g.ctx, g.c, g.vm, argv)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("mkdir -p in the guest exited %d: %s", code, out)
		}
	}
	return nil
}

// guestRun runs argv in vm's guest as root and returns its exit code and
// the start of its output.
func guestRun(ctx context.Context, c caller, vm string, argv []string) (int, discard, error) {
	var job api.JobStarted
	if err := call(ctx, c, api.MethodExecStart, api.ExecParams{VM: vm, Argv: argv}, &job); err != nil {
		return 0, nil, err
	}
	var out discard
	code, err := follow(ctx, c, vm, job.JobID, &out)
	return code, out, err
}

// discard keeps the start of what it is given, for an error message.
type discard []byte

func (d *discard) Write(b []byte) (int, error) {
	*d = append(*d, b[:min(len(b), 4096-len(*d))]...)
	return len(b), nil
}

func runCp(ctx context.Context, c *rpc.Client, src, dst cpPath) error {
	var res copied
	var err error
	host, vm := src, dst.vm
	if src.vm != "" {
		host, vm = dst, src.vm
	}
	if err := allowCopy(ctx, c, vm, host.path, src.vm != ""); err != nil {
		return err
	}
	if src.vm != "" {
		res, err = fromGuest(daemonGuest{ctx, c, src.vm}, src.path, dst.path)
	} else {
		res, err = toGuest(daemonGuest{ctx, c, dst.vm}, src.path, dst.path)
	}
	for _, s := range res.skipped {
		fmt.Fprintln(os.Stderr, "skipped", s)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%d files, %d bytes\n", res.files, res.bytes)
	return nil
}
