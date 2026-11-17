package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/rpc"
	"github.com/akunzai/glov/windows/internal/share"
)

// liveDaemon answers vm.status and share.list for one VM as glovd does,
// a missing VM or share as the *proto.Error rpc.Client returns.
type liveDaemon struct {
	deleted bool
	state   string
	disk    int
	shares  []share.Share
	// down fails every call as a connection that ended.
	down bool
}

func (d *liveDaemon) Call(ctx context.Context, method string, params, result any) error {
	if d.down {
		return rpc.ErrClosed
	}
	if d.deleted {
		return &proto.Error{Code: proto.CodeNotFound, Message: "no VM named dev"}
	}
	var res any
	switch method {
	case api.MethodVMStatus:
		res = api.VMInfo{Name: "dev", State: d.state, Disk: d.disk}
	case api.MethodShareList:
		list := []api.ShareInfo{}
		for _, s := range d.shares {
			list = append(list, api.NewShareInfo(s))
		}
		res = list
	default:
		return fmt.Errorf("unexpected %s", method)
	}
	b, _ := json.Marshal(res)
	return json.Unmarshal(b, result)
}

func TestLiveStateEndsSessionOnlyForGoodReasons(t *testing.T) {
	sessions := share.Share{Name: "sessions", HostPath: `C:\s`, Mode: share.Live}
	moved := sessions
	moved.HostPath = `C:\t`
	other := sessions
	other.Name = "other"
	for _, tc := range []struct {
		name     string
		d        liveDaemon
		gone     bool
		glovdErr bool
	}{
		{"running with its share", liveDaemon{state: "running", disk: 3, shares: []share.Share{sessions}}, false, false},
		// Stopped and deleted between two polls: glovd answers not found,
		// never stopped.
		{"VM deleted", liveDaemon{deleted: true}, true, false},
		{"VM stopped", liveDaemon{state: "stopped", disk: 3, shares: []share.Share{sessions}}, true, false},
		{"share removed", liveDaemon{state: "running", disk: 3, shares: []share.Share{other}}, true, false},
		{"share added again with another folder", liveDaemon{state: "running", disk: 3, shares: []share.Share{moved}}, true, false},
		{"glovd away", liveDaemon{down: true}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, gone, err := liveState(t.Context(), &tc.d, "dev", api.NewShareInfo(sessions))
			if (gone != nil) != tc.gone {
				t.Errorf("gone = %v, want gone %v", gone, tc.gone)
			}
			if (err != nil) != tc.glovdErr {
				t.Errorf("err = %v, want an error %v", err, tc.glovdErr)
			}
			if !tc.gone && !tc.glovdErr && info.Disk != 3 {
				t.Errorf("disk = %d, want 3", info.Disk)
			}
		})
	}
}
