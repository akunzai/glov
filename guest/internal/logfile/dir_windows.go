package logfile

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protectedDACL: SYSTEM and Administrators only, no inheritance from the parent.
const protectedDACL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// PrepareDir creates dir with a protected DACL, or, if it already exists,
// accepts it only when verifyDir finds it a real directory that nobody but
// SYSTEM and Administrators owns or can write to. Users may create folders
// under C:\ProgramData, so a pre-planted one must not receive SYSTEM's writes.
func PrepareDir(dir string) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(protectedDACL)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	if err := windows.CreateDirectory(p, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	return verifyDir(p)
}

func verifyDir(p *uint16) error {
	// Open the directory itself, not what a reparse point would lead to.
	h, err := windows.CreateFile(p, windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("is a reparse point (junction or symlink)")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("is not a directory")
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL() // ERROR_OBJECT_NOT_FOUND: no DACL at all
	var aces []ace
	if err == nil && dacl != nil {
		for i := range uint32(dacl.AceCount) {
			var a *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &a); err != nil {
				return err
			}
			if a.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE {
				aces = append(aces, ace{(*windows.SID)(unsafe.Pointer(&a.SidStart)).String(), uint32(a.Mask)})
			}
		}
	}
	if perr := checkPolicy(owner.String(), err == nil && dacl != nil, aces); perr != nil {
		return fmt.Errorf("unsafe log directory: %w", perr)
	}
	return nil
}
