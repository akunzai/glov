// Package winsys holds small Windows helpers shared by the Guest Service's packages.
package winsys

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// EnablePrivilege enables a privilege (for example "SeShutdownPrivilege") in
// the current process token. A service running as LocalSystem holds most
// privileges but many are present-and-disabled until enabled this way.
func EnablePrivilege(name string) error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return err
	}
	defer tok.Close()
	var luid windows.LUID
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	if err := windows.AdjustTokenPrivileges(tok, false, &tp, uint32(unsafe.Sizeof(tp)), nil, nil); err != nil {
		return err
	}
	// AdjustTokenPrivileges succeeds with ERROR_NOT_ALL_ASSIGNED when the token lacks the privilege.
	return windows.GetLastError()
}
