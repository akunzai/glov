package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
)

// newGuest is the Guest Service the test daemon ships.
const newGuest = "new guest"

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// fakeGuestFiles answers sha256sum, chmod, rm and the detached install the
// way the guest would, with installed as the installed Guest Service.
func fakeGuestFiles(f *fakeRunner, installed *string) func(argv []string) (string, int) {
	return func(argv []string) (string, int) {
		switch argv[0] {
		case "sha256sum":
			switch argv[1] {
			case guestInstalled:
				return sum(*installed) + "  " + argv[1] + "\n", 0
			case guestStaged:
				return sum(string(f.staged)) + "  " + argv[1] + "\n", 0
			}
		case "chmod", "rm":
			return "", 0
		case "systemd-run":
			// f.mu is held by ExecRead.
			*installed = string(f.staged)
			f.conn = "2"
			return "", 0
		}
		return "unexpected " + strings.Join(argv, " "), 127
	}
}

func TestGuestCheckAndUpdate(t *testing.T) {
	f, call := lifecycle(t)
	installed := "old guest"
	f.execOut = fakeGuestFiles(f, &installed)
	if err := call(api.MethodGuestCheck, api.VMParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "glov vm start dev") {
		t.Errorf("check on a stopped VM err = %v", err)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	var c api.GuestCheck
	if err := call(api.MethodGuestCheck, api.VMParams{VM: "dev"}, &c); err != nil || c.State != api.UpdateAvailable || *c.TargetVersion != "test" {
		t.Fatalf("guest.check = %+v, %v", c, err)
	}
	var vm api.VMInfo
	if err := call(api.MethodVMStatus, api.VMParams{VM: "dev"}, &vm); err != nil || vm.GuestService.Update == nil || vm.GuestService.Update.State != api.UpdateAvailable {
		t.Fatalf("vm.status = %+v, %v", vm.GuestService, err)
	}

	// Running jobs refuse an update unless forced.
	f.mu.Lock()
	f.jobs = 2
	f.mu.Unlock()
	err := call(api.MethodGuestUpdate, api.GuestUpdateParams{VM: "dev"}, nil)
	if e, ok := errors.AsType[*proto.Error](err); !ok || e.Code != proto.CodeInvalidParams || !strings.Contains(err.Error(), "2 exec jobs") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("update while busy err = %v", err)
	}
	if installed != "old guest" {
		t.Fatal("a refused update installed")
	}
	var u api.GuestUpdated
	if err := call(api.MethodGuestUpdate, api.GuestUpdateParams{VM: "dev", Force: true, Timeout: 5}, &u); err != nil || !u.Updated {
		t.Fatalf("guest.update --force = %+v, %v", u, err)
	}
	if installed != newGuest {
		t.Errorf("installed %q", installed)
	}
	if f.ExecRunning(devID) != 0 {
		t.Error("the jobs the update cut off still run")
	}
	if err := call(api.MethodVMStatus, api.VMParams{VM: "dev"}, &vm); err != nil || vm.GuestService.Update.State != api.UpdateUpToDate {
		t.Fatalf("vm.status after the update = %+v, %v", vm.GuestService.Update, err)
	}
	// A second update changes nothing.
	f.mu.Lock()
	f.jobs = 0
	f.mu.Unlock()
	if err := call(api.MethodGuestUpdate, api.GuestUpdateParams{VM: "dev"}, &u); err != nil || u.Updated {
		t.Fatalf("second guest.update = %+v, %v", u, err)
	}
	if err := call(api.MethodGuestUpdate, api.GuestUpdateParams{VM: "dev", Timeout: 7200}, nil); err == nil {
		t.Error("a timeout over an hour was accepted")
	}
}

func TestGuestUpdateTimesOutWithoutANewConnection(t *testing.T) {
	f, call := lifecycle(t)
	installed := "old guest"
	files := fakeGuestFiles(f, &installed)
	f.execOut = func(argv []string) (string, int) {
		if argv[0] == "systemd-run" {
			return "", 0 // the installer never restarts the Guest Service
		}
		return files(argv)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	err := call(api.MethodGuestUpdate, api.GuestUpdateParams{VM: "dev", Timeout: 0.5}, nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v", err)
	}
}

func TestStats(t *testing.T) {
	f, call := lifecycle(t)
	if err := call(api.MethodVMStats, api.StatsParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("stats of a stopped VM err = %v", err)
	}
	var s api.Stats
	if err := call(api.MethodVMStats, api.StatsParams{}, &s); err != nil || len(s.VMs) != 0 || s.Host.Physical != 32<<30 {
		t.Fatalf("vm.stats with nothing running = %+v, %v", s, err)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(api.MethodVMStats, api.StatsParams{}, &s); err != nil || len(s.VMs) != 1 {
		t.Fatalf("vm.stats = %+v, %v", s, err)
	}
	v := s.VMs[0]
	if v.Name != "dev" || v.Memory != 1<<30 || v.DiskSize != 8<<30 || v.CPUPercent == nil || *v.MemoryResident != 1<<30 || v.Guest != nil ||
		v.GuestProblem == nil || !strings.Contains(*v.GuestProblem, "glov vm guest update dev") {
		t.Errorf("row %+v", v)
	}
	f.guestStats = &api.GuestStats{MemoryTotal: 1 << 30, DiskPath: "/", Load: []float64{0.5, 0.25, 0}}
	if err := call(api.MethodVMStats, api.StatsParams{VM: "de"}, &s); err != nil || s.VMs[0].Guest == nil || s.VMs[0].Guest.Load[0] != 0.5 {
		t.Fatalf("vm.stats dev = %+v, %v", s, err)
	}
}

func TestLaunchInstallLeavesTheServiceCgroupOnSystemd(t *testing.T) {
	if got := launchInstall(); got[0] != "systemd-run" || !slices.Contains(got, guestStaged) {
		t.Errorf("launchInstall() = %q", got)
	}
}
