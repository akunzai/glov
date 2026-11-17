package guestservice

import (
	"github.com/akunzai/glov/guest/internal/winsys"
	"testing"
	"time"
)

func TestHostInfoOnWindows(t *testing.T) {
	host, user, boot := hostInfo()
	if host == "" {
		t.Error("empty hostname")
	}
	if _, err := time.Parse(time.RFC3339Nano, boot); err != nil {
		t.Errorf("bootId %q is not a timestamp: %v", boot, err)
	}
	if again := bootTime(); again != boot {
		t.Errorf("bootId changed between calls: %q vs %q", boot, again)
	}
	if boot > time.Now().UTC().Format(time.RFC3339Nano) {
		t.Errorf("bootId %q is in the future", boot)
	}
	if user != nil {
		t.Logf("console user: %s", *user)
	}
	t.Logf("host=%s boot=%s", host, boot)
}

func TestEnablePrivilegeKeepsWorking(t *testing.T) {
	// SeChangeNotifyPrivilege is held by every account.
	if err := winsys.EnablePrivilege("SeChangeNotifyPrivilege"); err != nil {
		t.Fatal(err)
	}
}
