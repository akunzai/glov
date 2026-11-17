package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

// guestErr names the VM in errors about reaching its guest; a Guest Service
// error (*proto.Error) passes through with its code, so glov exec can tell
// "nobody signed in" (4) apart.
func guestErr(r store.Record, err error) error {
	if _, ok := errors.AsType[*proto.Error](err); ok {
		return err
	}
	if errors.Is(err, vmrun.ErrNotRunning) {
		return fmt.Errorf("VM %s is not running: glov vm start %s", r.Name, r.Name)
	}
	if errors.Is(err, vmrun.ErrNoJob) {
		return proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	return fmt.Errorf("VM %s: %w", r.Name, err)
}

func (d *Daemon) execStart(ctx context.Context, caller string, p api.ExecParams) (api.JobStarted, error) {
	if len(p.Argv) == 0 || p.Argv[0] == "" {
		return api.JobStarted{}, proto.Errorf(proto.CodeInvalidParams, "give a command to run")
	}
	if p.As != "" && p.As != "system" && p.As != "user" {
		return api.JobStarted{}, proto.Errorf(proto.CodeInvalidParams, `as must be "system" or "user"`)
	}
	r, err := d.load(caller, p.VM)
	if err != nil {
		return api.JobStarted{}, err
	}
	id, err := d.VMs.ExecStart(ctx, r.ID, vmrun.ExecParams{Argv: p.Argv, Cwd: p.Cwd, Env: p.Env, As: p.As})
	if err != nil {
		return api.JobStarted{}, guestErr(r, err)
	}
	return api.JobStarted{JobID: id}, nil
}

func (d *Daemon) execRead(caller string, p api.ExecReadParams) (api.ExecRead, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return api.ExecRead{}, err
	}
	if p.MaxBytes <= 0 || p.MaxBytes > api.MaxReadBytes {
		p.MaxBytes = api.MaxReadBytes
	}
	if p.Cursor < 0 {
		return api.ExecRead{}, proto.Errorf(proto.CodeInvalidParams, "cursor must not be negative")
	}
	res, err := d.VMs.ExecRead(r.ID, p.JobID, p.Cursor, p.MaxBytes)
	if err != nil {
		return api.ExecRead{}, guestErr(r, err)
	}
	return api.ExecRead{Data: res.Data, Cursor: res.Cursor, Exited: res.Exited, Code: res.Code, Lost: res.Lost, Dropped: res.Dropped}, nil
}

func (d *Daemon) execKill(ctx context.Context, caller string, p api.ExecKillParams) error {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return err
	}
	if err := d.VMs.ExecKill(ctx, r.ID, p.JobID); err != nil {
		return guestErr(r, err)
	}
	return nil
}

func (d *Daemon) guestCall(ctx context.Context, caller string, p api.GuestCallParams) (jsontext.Value, error) {
	if !vmrun.FileMethods[p.Method] {
		return nil, proto.Errorf(proto.CodeInvalidParams, "guest.call passes on only the Guest Service's file.stat, file.list, file.read and file.write")
	}
	r, err := d.load(caller, p.VM)
	if err != nil {
		return nil, err
	}
	var res jsontext.Value
	if err := d.VMs.GuestCall(ctx, r.ID, p.Method, p.Params, &res); err != nil {
		return nil, guestErr(r, err)
	}
	return res, nil
}
