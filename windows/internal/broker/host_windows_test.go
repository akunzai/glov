package broker

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/windows/internal/winsec"
)

// selfPipe returns the server end of a pipe this process dialed at
// impersonation level, as glovd dials the broker.
func selfPipe(t *testing.T) net.Conn {
	t.Helper()
	name := `\\.\pipe\glov-broker-test-` + strings.ReplaceAll(t.Name(), "/", "-")
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := winio.DialPipeAccessImpLevel(ctx, name, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelImpersonation)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	server := <-accepted
	t.Cleanup(func() { server.Close() })
	return server
}

// ownBy makes sid the owner of paths: what an elevated administrator
// creates belongs to Administrators, as on CI, while glovd, never
// elevated, makes disks its user owns.
func ownBy(t *testing.T, sid string, paths ...string) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, s, nil, nil, nil); err != nil {
			t.Fatalf("own %s: %v", p, err)
		}
	}
}

func TestOpenDiskAcceptsTheCallersDiskAndPinsItsFolder(t *testing.T) {
	me, err := winsec.CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "vms")
	disk := filepath.Join(root, "dev", "disk.vhdx")
	os.MkdirAll(filepath.Dir(disk), 0o700)
	os.WriteFile(disk, []byte("x"), 0o600)
	ownBy(t, me, root, filepath.Dir(disk), disk)
	c := selfPipe(t)
	release, err := openDisk(c, me, root, disk)
	if err != nil {
		t.Fatalf("openDisk: %v", err)
	}
	if err := os.Rename(filepath.Join(root, "dev"), filepath.Join(root, "moved")); err == nil {
		t.Error("renamed a pinned folder")
	}
	release()
	if err := os.Rename(filepath.Join(root, "dev"), filepath.Join(root, "moved")); err != nil {
		t.Errorf("rename after release: %v", err)
	}
	if _, err := openDisk(c, "S-1-5-21-1-2-3-4", root, filepath.Join(root, "moved", "disk.vhdx")); err == nil {
		t.Error("accepted a disk owned by someone else")
	}
}

func TestOpenDiskRefusesAJunction(t *testing.T) {
	me, err := winsec.CurrentSID()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "vms")
	target := filepath.Join(base, "elsewhere")
	os.MkdirAll(root, 0o700)
	os.MkdirAll(target, 0o700)
	os.WriteFile(filepath.Join(target, "disk.vhdx"), []byte("x"), 0o600)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "dev"), target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if _, err := openDisk(selfPipe(t), me, root, filepath.Join(root, "dev", "disk.vhdx")); err == nil || !strings.Contains(err.Error(), "junction") {
		t.Fatalf("openDisk through a junction = %v", err)
	}
}
