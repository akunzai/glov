package daemon

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

// console reads the VM's serial console log, which only SYSTEM and
// administrators can open in its directory (decision 6 in
// akunzai/glov#146). The log starts again on every boot, with a new
// creation time: a read for another boot's log, or past this one's end,
// reads it from the start.
func (d *Daemon) console(caller string, p api.ConsoleParams) (api.ConsoleRead, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return api.ConsoleRead{}, err
	}
	maxBytes := p.MaxBytes
	if maxBytes <= 0 || maxBytes > api.MaxConsoleRead {
		maxBytes = api.MaxConsoleRead
	}
	f, err := os.Open(filepath.Join(d.Store.Dir(r.Name), vmrun.ConsoleLog))
	if errors.Is(err, fs.ErrNotExist) {
		return api.ConsoleRead{}, proto.Errorf(proto.CodeInvalidParams, "VM %s has no console log: it has not started since it was created", r.Name)
	}
	if err != nil {
		return api.ConsoleRead{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return api.ConsoleRead{}, err
	}
	res := api.ConsoleRead{Offset: p.Offset, Running: d.VMs.Running(r.ID)}
	res.Log = logID(fi)
	if p.Offset < 0 || p.Offset > fi.Size() || (p.Log != "" && p.Log != res.Log) {
		res.Offset, res.Restarted = 0, p.Log != "" || p.Offset != 0
	}
	buf := make([]byte, min(int64(maxBytes), fi.Size()-res.Offset))
	n, err := f.ReadAt(buf, res.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return api.ConsoleRead{}, err
	}
	res.Data = buf[:n]
	res.Offset += int64(n)
	return res, nil
}
