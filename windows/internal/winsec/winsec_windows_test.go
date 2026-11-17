package winsec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/windows/internal/hcs"
)

func needElevation(t *testing.T) {
	t.Helper()
	if !Elevated() {
		t.Skip("needs an elevated shell: the data directory is owned by Administrators")
	}
}

func TestDataDirCreatedProtectedAndAccepted(t *testing.T) {
	needElevation(t)
	dir := filepath.Join(t.TempDir(), "data")
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	// Creating it again keeps it acceptable.
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDataDirWithExtraAccessIsRefusedThenRepaired(t *testing.T) {
	needElevation(t)
	dir := filepath.Join(t.TempDir(), "data")
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)M").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if err := CheckDataDir(dir, ""); err == nil || !strings.Contains(err.Error(), "S-1-5-32-545") {
		t.Fatalf("CheckDataDir = %v, want it to name the Users group", err)
	}
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir, ""); err != nil {
		t.Fatalf("after repair: %v", err)
	}
}

func TestUserDataDirIsTheUsersAndRefusesOtherUsers(t *testing.T) {
	me, err := CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "glov")
	if err := CreateDataDir(dir, me); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "vms", "dev"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir, me); err != nil {
		t.Fatalf("CheckDataDir for its owner: %v", err)
	}
	if err := CheckDataDir(dir, "S-1-5-21-1-2-3-4"); err == nil {
		t.Fatal("CheckDataDir accepted a directory another user owns")
	}
	if out, err := exec.Command("icacls", filepath.Join(dir, "vms"), "/grant", "*S-1-5-32-545:(OI)(CI)M").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if err := CheckDataDir(dir, me); err == nil || !strings.Contains(err.Error(), "S-1-5-32-545") {
		t.Fatalf("CheckDataDir = %v, want it to name the Users group", err)
	}
	if err := CreateDataDir(dir, me); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir, me); err != nil {
		t.Fatalf("after repair: %v", err)
	}
}

// Windows' ssh-keygen writes a .pub with its own ACL that lets Everyone
// read it; glovd refused to start on a glov home holding one.
func TestProtectFileDropsAnotherProgramsACL(t *testing.T) {
	me, err := CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "glov")
	if err := CreateDataDir(dir, me); err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAA glov@dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", pub, "/inheritance:r", "/grant", "*S-1-1-0:RX", "/grant", "*"+me+":M").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if err := CheckDataDir(dir, me); err == nil || !strings.Contains(err.Error(), "S-1-1-0") {
		t.Fatalf("CheckDataDir = %v, want it to name Everyone", err)
	}
	if err := ProtectFile(pub, me); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir, me); err != nil {
		t.Fatalf("after ProtectFile: %v", err)
	}
	if b, err := os.ReadFile(pub); err != nil || len(b) == 0 {
		t.Fatalf("read %s after ProtectFile: %v", pub, err)
	}
}

// ssh-keygen gives its private key Modify only, without WRITE_OWNER; the
// repair still resets its DACL.
func TestRepairResetsAFileWithoutWriteOwner(t *testing.T) {
	me, err := CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "glov")
	if err := CreateDataDir(dir, me); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", key, "/inheritance:r", "/grant", "*S-1-1-0:R", "/grant", "*"+me+":M").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if err := CreateDataDir(dir, me); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if err := CheckDataDir(dir, me); err != nil {
		t.Fatalf("after repair: %v", err)
	}
}

func TestDataDirOwnedByAUserIsRefused(t *testing.T) {
	dir := t.TempDir() // inherits the user's profile ACL and owner
	if err := CheckDataDir(dir, ""); err == nil {
		t.Fatal("CheckDataDir accepted a user's temp directory")
	}
}

func TestJunctionAsDataDirIsRefused(t *testing.T) {
	needElevation(t)
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := CreateDataDir(target, ""); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	if err := CheckDataDir(link, ""); err == nil {
		t.Fatal("CheckDataDir accepted a junction")
	}
	if err := CreateDataDir(link, ""); err == nil {
		t.Fatal("CreateDataDir reset the ACL through a junction")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
}

func TestPipeClientAndOwnerIdentity(t *testing.T) {
	me, err := CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	path := `\\.\pipe\glov-winsec-test-` + strings.ReplaceAll(t.Name(), "/", "-")
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: "O:" + me + "D:P(A;;GA;;;" + me + ")"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- err.Error()
			return
		}
		defer c.Close()
		sid, err := PipeClientSID(c)
		if err != nil {
			sid = err.Error()
		}
		got <- sid
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := winio.DialPipeAccessImpLevel(ctx, path, uint32(0x80000000|0x40000000), winio.PipeImpLevelIdentification)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if owner, err := PipeOwner(c); err != nil || owner != me {
		t.Fatalf("PipeOwner = %q, %v; want %s", owner, err, me)
	}
	if sid := <-got; sid != me {
		t.Fatalf("PipeClientSID = %q, want %s", sid, me)
	}
}

func setOwner(t *testing.T, path, sid string) {
	t.Helper()
	if out, err := exec.Command("icacls", path, "/setowner", "*"+sid).CombinedOutput(); err != nil {
		t.Fatalf("icacls /setowner: %v %s", err, out)
	}
}

// %ProgramData% lets any user create folders: one that a user created before
// the install must not be adopted with whatever they put inside.
func TestPreexistingDirectoryOwnedByAUserIsNotAdopted(t *testing.T) {
	needElevation(t)
	me, _ := CurrentSID()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setOwner(t, dir, me)
	err := CreateDataDir(dir, "")
	if err == nil || !strings.Contains(err.Error(), "not created by glov host install") {
		t.Fatalf("CreateDataDir = %v, want a refusal", err)
	}
}

func TestEntryOwnedByAUserInsideIsRefused(t *testing.T) {
	needElevation(t)
	me, _ := CurrentSID()
	dir := filepath.Join(t.TempDir(), "data")
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, "vms")
	if err := os.Mkdir(planted, 0o700); err != nil {
		t.Fatal(err)
	}
	setOwner(t, planted, me)
	if err := CheckDataDir(dir, ""); err == nil || !strings.Contains(err.Error(), "vms is owned by") {
		t.Fatalf("CheckDataDir = %v, want the planted entry named", err)
	}
	if err := CreateDataDir(dir, ""); err == nil {
		t.Fatal("CreateDataDir repaired around a planted entry")
	}
}

func TestJunctionInsideDataDirIsRefused(t *testing.T) {
	needElevation(t)
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(dir, "vms"), base).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	if err := CheckDataDir(dir, ""); err == nil || !strings.Contains(err.Error(), "junction") {
		t.Fatalf("CheckDataDir = %v, want the junction refused", err)
	}
}

func TestRepairResetsEntriesThatInheritedExtraAccess(t *testing.T) {
	needElevation(t)
	dir := filepath.Join(t.TempDir(), "data")
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	// A file glovd wrote, so owned by SYSTEM (Administrators here).
	log := filepath.Join(dir, "glovd.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	setOwner(t, log, Administrators)
	if out, err := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)M", "/T").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if err := CheckDataDir(dir, ""); err == nil {
		t.Fatal("CheckDataDir accepted extra access")
	}
	if err := CreateDataDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(dir, ""); err != nil {
		t.Fatalf("after repair: %v", err)
	}
}

func TestPinnedDirCannotBeMovedOrDeleted(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "parent", "pinned")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pin, err := PinDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"2"); err == nil {
		t.Error("renamed a pinned directory")
	}
	if err := os.Rename(filepath.Dir(dir), filepath.Dir(dir)+"2"); err == nil {
		t.Error("renamed the parent of a pinned directory")
	}
	if err := os.Remove(dir); err == nil {
		t.Error("deleted a pinned directory")
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
		t.Errorf("writing inside a pinned directory: %v", err)
	}
	pin.Close()
	if err := os.Rename(filepath.Dir(dir), filepath.Dir(dir)+"2"); err != nil {
		t.Errorf("rename after unpinning: %v", err)
	}
}

func TestPinRefusesAJunction(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	os.Mkdir(target, 0o700)
	link := filepath.Join(base, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	if pin, err := PinDir(link); err == nil {
		pin.Close()
		t.Fatal("PinDir accepted a junction")
	}
}

func TestVMAccessIsAllowedOnlyOnVMDisks(t *testing.T) {
	needElevation(t)
	const vm = "6f1c2a9e-0c5d-4b8e-9a3f-1d2e3f4a5b6c"
	for _, tc := range []struct {
		name string
		dir  bool
		ok   bool
	}{
		{"disk.vhdx", false, true},
		{"seed.vhd", false, true},
		{"vm.json", false, false},
		{"folder.vhdx", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "data")
			if err := CreateDataDir(root, ""); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, tc.name)
			if tc.dir {
				os.Mkdir(path, 0o700)
			} else {
				os.WriteFile(path, nil, 0o600)
			}
			setOwner(t, path, Administrators)
			if err := hcs.GrantVmAccess(vm, path); err != nil {
				t.Fatal(err)
			}
			if err := CheckDataDir(root, ""); (err == nil) != tc.ok {
				t.Fatalf("CheckDataDir after granting a VM %s: %v", tc.name, err)
			}
		})
	}
}

func TestUserAccessOnAVMDiskIsRefused(t *testing.T) {
	needElevation(t)
	root := filepath.Join(t.TempDir(), "data")
	if err := CreateDataDir(root, ""); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(root, "disk.vhdx")
	os.WriteFile(disk, nil, 0o600)
	setOwner(t, disk, Administrators)
	me, _ := CurrentSID()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + me + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetNamedSecurityInfo(disk, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckDataDir(root, ""); err == nil {
		t.Fatal("a user's ACE on a VM disk was accepted")
	}
}
