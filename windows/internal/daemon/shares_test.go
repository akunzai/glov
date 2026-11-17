package daemon

import (
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/harness"
	"github.com/akunzai/glov/windows/internal/share"
)

func TestShareAddListRemove(t *testing.T) {
	f, call := lifecycle(t)
	var info api.ShareInfo
	add := func(s share.Share) error {
		return call(api.MethodShareAdd, api.ShareAddParams{VM: "de", Share: s}, &info)
	}
	if err := add(share.Share{Name: "proj", HostPath: `C:\src\Proj`, Exclude: []string{"bin/"}}); err != nil {
		t.Fatal(err)
	}
	if info.GuestPath != "/mnt/c/src/Proj" || info.Mode != share.RW {
		t.Errorf("share.add = %+v", info)
	}
	if err := add(share.Share{Name: "ref", HostPath: `D:\ref`, Mode: share.RO}); err != nil {
		t.Fatal(err)
	}
	type addCase struct {
		s    share.Share
		want string
	}
	var protected []addCase
	// The test's data directory is a host temp directory, a drive path only
	// on Windows.
	if runtime.GOOS == "windows" {
		protected = []addCase{
			{share.Share{Name: "data", HostPath: filepath.Join(f.st.Root, "vms")}, "glovd's data directory"},
			{share.Share{Name: "up", HostPath: filepath.Dir(f.st.Root)}, "glovd's data directory"},
			// Only the VM's own conversation records folder, under that share's name.
			{share.Share{Name: "records", HostPath: filepath.Join(f.st.Root, harness.SessionsFolder, "dev")}, "glovd's data directory"},
			{share.Share{Name: harness.SessionsShare, HostPath: filepath.Join(f.st.Root, harness.SessionsFolder, "other")}, "glovd's data directory"},
			{share.Share{Name: harness.SessionsShare, HostPath: filepath.Join(f.st.Root, harness.SessionsFolder)}, "glovd's data directory"},
		}
	}
	for _, c := range append(protected, []addCase{
		{share.Share{Name: "proj", HostPath: `E:\x`}, "already has a share named proj"},
		{share.Share{Name: "in", HostPath: `c:\SRC\proj\sub`}, "overlaps share proj"},
		{share.Share{Name: "root", HostPath: `C:\`}, "drive root"},
		{share.Share{Name: "unc", HostPath: `\\server\share`}, "not an absolute path on a drive"},
		{share.Share{Name: "lit", HostPath: `E:\x`, Exclude: []string{"a[b"}}, ""},
	}...) {
		err := add(c.s)
		if c.want == "" {
			if err != nil {
				t.Errorf("add %+v: %v", c.s, err)
			}
			continue
		}
		if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams || !strings.Contains(e.Message, c.want) {
			t.Errorf("add %+v = %v, want %q", c.s, err, c.want)
		}
	}
	var list []api.ShareInfo
	if err := call(api.MethodShareList, api.VMParams{VM: "dev"}, &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"proj", "ref", "lit"}) || list[1].GuestPath != "/mnt/d/ref" || list[1].Mode != share.RO {
		t.Errorf("share.list = %+v", list)
	}
	if err := call(api.MethodShareRemove, api.ShareParams{VM: "dev", Name: "ref"}, nil); err != nil {
		t.Fatal(err)
	}
	err := call(api.MethodShareRemove, api.ShareParams{VM: "dev", Name: "ref"}, nil)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeNotFound || !strings.Contains(e.Message, "shares: proj, lit") {
		t.Errorf("remove a missing share = %v", err)
	}
	me := testOwner
	r, err := f.st.Load(me, "dev")
	if err != nil || len(r.Shares) != 2 {
		t.Errorf("vm.json shares = %+v, %v", r.Shares, err)
	}
	// Another user's VM is not found.
	if err := call(api.MethodShareList, api.VMParams{VM: "other"}, &list); err == nil {
		t.Error("listed another user's shares")
	}
}

// glov claude keeps a VM's conversation records in <data>\sessions\<vm>,
// %LOCALAPPDATA%\glov being glovd's data directory too.
func TestTheVMsOwnRecordsFolderMayBeShared(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the test's data directory is a drive path only on Windows")
	}
	f, call := lifecycle(t)
	s := share.Share{Name: harness.SessionsShare, HostPath: strings.ToUpper(filepath.Join(f.st.Root, harness.SessionsFolder, "dev")), Mode: share.Live}
	var info api.ShareInfo
	if err := call(api.MethodShareAdd, api.ShareAddParams{VM: "dev", Share: s}, &info); err != nil {
		t.Fatal(err)
	}
	if info.Mode != share.Live {
		t.Errorf("share.add = %+v", info)
	}
}
