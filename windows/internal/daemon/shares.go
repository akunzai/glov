package daemon

import (
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/share"
)

// protected are glovd's own directories, which no share may touch: a
// share is part of the copy allowlist. Except VM vm's conversation records
// share, which glov claude makes at <data>\sessions\<vm>: the data
// directory is %LOCALAPPDATA%\glov, and that folder holds no VM's files.
func (d *Daemon) protected(vm string, s share.Share) []share.Protected {
	data := share.Protected{Path: d.Store.Root, Tree: true, Why: "glovd's data directory, which holds VM disks and secrets"}
	if records := filepath.Join(d.Store.Root, harness.SessionsFolder, vm); s.Name == harness.SessionsShare &&
		share.Within(s.HostPath, records) && share.Within(records, s.HostPath) {
		data.Path = ""
	}
	return []share.Protected{data, {Path: d.Cache, Tree: true, Why: "glovd's image cache"}}
}

// shareAdd records a share in the VM's vm.json. It may run while the VM
// does: nothing is attached, a sync reads the record when it starts.
func (d *Daemon) shareAdd(caller string, p api.ShareAddParams) (api.ShareInfo, error) {
	r, unlock, err := d.locked(caller, p.VM)
	if err != nil {
		return api.ShareInfo{}, err
	}
	defer unlock()
	s, err := share.Check(p.Share, r.Shares, d.protected(r.Name, p.Share))
	if err != nil {
		return api.ShareInfo{}, proto.Errorf(proto.CodeInvalidParams, "%v", err)
	}
	r.Shares = append(slices.Clone(r.Shares), s)
	if err := d.Store.Save(r); err != nil {
		return api.ShareInfo{}, err
	}
	slog.Info("share added", "vm", r.Name, "share", s.Name, "hostPath", s.HostPath, "mode", s.Mode)
	return api.NewShareInfo(s), nil
}

// shareRemove drops a share from vm.json; what a sync put in the guest stays.
func (d *Daemon) shareRemove(caller string, p api.ShareParams) error {
	r, unlock, err := d.locked(caller, p.VM)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := share.Find(r.Shares, p.Name); err != nil {
		return proto.Errorf(proto.CodeNotFound, "%v", err)
	}
	r.Shares = slices.DeleteFunc(slices.Clone(r.Shares), func(s share.Share) bool { return s.Name == p.Name })
	if err := d.Store.Save(r); err != nil {
		return err
	}
	slog.Info("share removed", "vm", r.Name, "share", p.Name)
	return nil
}

func (d *Daemon) shareList(caller string, p api.VMParams) ([]api.ShareInfo, error) {
	r, err := d.load(caller, p.VM)
	if err != nil {
		return nil, err
	}
	list := make([]api.ShareInfo, 0, len(r.Shares))
	for _, s := range r.Shares {
		list = append(list, api.NewShareInfo(s))
	}
	return list, nil
}
