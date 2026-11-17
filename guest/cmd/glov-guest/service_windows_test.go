package main

import (
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestUpdatedConfigKeepsServiceType(t *testing.T) {
	cur := mgr.Config{
		ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, ErrorControl: mgr.ErrorIgnore,
		StartType: mgr.StartManual, BinaryPathName: `"C:\Program Files\glov\glov-guest.exe"`,
		ServiceStartName: "LocalSystem", DisplayName: "old", Description: "old",
	}
	got := updatedConfig(cur)
	if got.ServiceType == 0 || got.ServiceType != cur.ServiceType {
		t.Errorf("ServiceType = %#x, want %#x kept (zero makes ChangeServiceConfig fail)", got.ServiceType, cur.ServiceType)
	}
	if got.ErrorControl != mgr.ErrorIgnore {
		t.Errorf("ErrorControl = %#x", got.ErrorControl)
	}
	if got.StartType != mgr.StartAutomatic || got.DisplayName != "glov Guest Service" || got.Description != serviceDescription {
		t.Errorf("owned fields not refreshed: %+v", got)
	}
	if got.BinaryPathName != cur.BinaryPathName || got.ServiceStartName != "LocalSystem" {
		t.Errorf("binary path or account changed: %+v", got)
	}
}

func TestBinaryPath(t *testing.T) {
	for in, want := range map[string]string{
		`"C:\Program Files\glov\glov-guest.exe"`:    `C:\Program Files\glov\glov-guest.exe`,
		`"C:\Program Files\glov\glov-guest.exe" -x`: `C:\Program Files\glov\glov-guest.exe`,
		`C:\glov-dev\glov-guest.exe`:                `C:\glov-dev\glov-guest.exe`,
	} {
		if got := binaryPath(in); got != want {
			t.Errorf("binaryPath(%q) = %q, want %q", in, got, want)
		}
	}
}
