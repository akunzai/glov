package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/jobrun"
	"github.com/akunzai/glov/windows/internal/store"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

// runGuest runs argv as root in r's guest and sends its output to out, line
// by line; it returns the exit code. Ending ctx kills the command.
func (d *Daemon) runGuest(ctx context.Context, r store.Record, argv []string, out func(string)) (int, error) {
	job, err := d.VMs.ExecStart(ctx, r.ID, vmrun.ExecParams{Argv: argv, As: "system"})
	if err != nil {
		return 0, guestErr(r, err)
	}
	lw := &lineWriter{out: out}
	res, err := jobrun.Run(ctx, vmJob{d, r, job}, lw, 0)
	if err != nil {
		return 0, err
	}
	lw.flush()
	if res.Dropped {
		slog.Warn("guest command output fell out of the window", "vm", r.Name, "argv0", argv[0])
	}
	if res.State == jobrun.Lost {
		return 0, fmt.Errorf("VM %s stopped or rebooted before the command ended", r.Name)
	}
	return res.Code, nil
}

// vmJob is a job glovd started in r's guest itself.
type vmJob struct {
	d  *Daemon
	r  store.Record
	id string
}

func (j vmJob) Read(_ context.Context, cursor int) (jobrun.Read, error) {
	rd, err := j.d.VMs.ExecRead(j.r.ID, j.id, cursor, api.MaxReadBytes)
	if err != nil {
		return jobrun.Read{}, guestErr(j.r, err)
	}
	return jobrun.Read{Data: rd.Data, Cursor: rd.Cursor, Exited: rd.Exited, Code: rd.Code, Lost: rd.Lost, Dropped: rd.Dropped}, nil
}

func (j vmJob) Kill(ctx context.Context) error {
	return j.d.VMs.ExecKill(ctx, j.r.ID, j.id)
}

// lineWriter hands out what it is given one line at a time, without the
// line ending; flush hands out a last line that has none.
type lineWriter struct {
	out     func(string)
	pending []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.pending = append(w.pending, b...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			return len(b), nil
		}
		w.out(string(bytes.TrimRight(w.pending[:i], "\r")))
		w.pending = w.pending[i+1:]
	}
}

func (w *lineWriter) flush() {
	if len(w.pending) > 0 {
		w.out(string(w.pending))
		w.pending = nil
	}
}
