// Package winsec holds the Windows security checks glovd and glov rely on:
// the service data directory's ACL, the identity of a pipe client and the
// owner of the pipe a client connects to.
package winsec

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// SYSTEM is LocalSystem's SID, the account glovd runs as.
	SYSTEM = "S-1-5-18"
	// Administrators is the BUILTIN\Administrators group.
	Administrators = "S-1-5-32-544"
)

// DataDirSDDL protects a data directory: for a service's (user ""), owned
// by Administrators with access for SYSTEM and Administrators only; for a
// user's glovd, owned by user with access for SYSTEM, Administrators and
// user, as a profile folder has. Not inherited from the parent, inherited
// by everything inside.
func DataDirSDDL(user string) string {
	if user == "" {
		return "O:BAG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	}
	return "O:" + user + "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user + ")"
}

// CreateDataDir creates dir with DataDirSDDL(user). An existing directory
// is repaired only if SYSTEM, an administrator or user created it: a
// folder another user could create, as in %ProgramData% or a shared drive,
// may hold planted files and is refused for an administrator to inspect.
// The checks and the repair act on one handle opened without following
// reparse points, so the path cannot be swapped for a junction in between.
func CreateDataDir(dir, user string) error {
	sd, err := windows.SecurityDescriptorFromString(DataDirSDDL(user))
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(p, sa)
	if err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	h, err := openNoFollow(dir, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := plainDir(h, dir); err != nil {
		return err
	}
	cur, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the owner of %s: %w", dir, err)
	}
	if err := trustedOwner(cur, dir, user); err != nil {
		return fmt.Errorf("%w; it was not created by glov host install: inspect it and remove it", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	info := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("reset the ACL of %s: %w", dir, err)
	}
	// Setting a DACL on a handle does not propagate it, so reset each entry
	// inside the same way, refusing any that someone else owns.
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return fmt.Errorf("unsafe data directory: %s is a link or junction (inspect it and remove it)", path)
		}
		entryInfo := info
		h, err := openNoFollow(path, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			// An ACL another program set may leave out WRITE_OWNER (Windows'
			// ssh-keygen gives its keys Modify); the owner may still reset
			// the DACL, and the owner is checked below.
			entryInfo &^= windows.OWNER_SECURITY_INFORMATION
			h, err = openNoFollow(path, windows.READ_CONTROL|windows.WRITE_DAC)
		}
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		var fi windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
			return err
		}
		if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("unsafe data directory: %s is a reparse point (inspect it and remove it)", path)
		}
		cur, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the owner of %s: %w", path, err)
		}
		if err := trustedOwner(cur, path, user); err != nil {
			return fmt.Errorf("%w: inspect it and remove it", err)
		}
		entryOwner := owner
		if entryInfo&windows.OWNER_SECURITY_INFORMATION == 0 {
			entryOwner = nil
		}
		if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, entryInfo, entryOwner, nil, dacl, nil); err != nil {
			return fmt.Errorf("reset the ACL of %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return checkTree(dir, user)
}

// ProtectFile gives a file user made in their glov home the DACL
// CreateDataDir gives everything there, replacing explicit ACEs another
// program set (Windows' ssh-keygen lets Everyone read a .pub), which
// CheckDataDir would refuse.
func ProtectFile(path, user string) error {
	sd, err := windows.SecurityDescriptorFromString(DataDirSDDL(user))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	h, err := openNoFollow(path, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("reset the ACL of %s: %w", path, err)
	}
	return nil
}

// CheckDataDir fails unless dir and everything in it are plain files and
// directories owned by SYSTEM, Administrators or user whose DACLs grant
// access to those only, with dir's DACL protected from the parent. glovd
// (user, its owner) and glov-broker (user "") run it at startup, so a
// directory another user could plant files in is never trusted.
func CheckDataDir(dir, user string) error {
	h, err := openNoFollow(dir, windows.READ_CONTROL)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := plainDir(h, dir); err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the ACL of %s: %w", dir, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("unsafe data directory %s: its DACL inherits from the parent (run glov host install again)", dir)
	}
	if err := trustedSD(sd, dir, false, user); err != nil {
		return err
	}
	return checkTree(dir, user)
}

// PinDir opens dir and keeps it open until the returned Closer closes. The
// handle shares reading and writing but not deletion, so while it is open
// nobody can rename or delete dir or any directory above it: a path checked
// once keeps naming the same directory. glovd pins its data and cache
// directories before checking them and keeps them pinned while it runs,
// which matters for a cache moved under a parent its user can write.
func PinDir(dir string) (io.Closer, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	// FILE_LIST_DIRECTORY makes the handle count in sharing checks; with
	// READ_CONTROL alone, a rename would still be allowed.
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_LIST_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	if err := plainDir(h, dir); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return pinned(h), nil
}

type pinned windows.Handle

func (h pinned) Close() error { return windows.CloseHandle(windows.Handle(h)) }

// checkTree checks every entry below dir. Nothing inside may be a reparse
// point, and each entry's owner and DACL must pass trustedSD: a file another
// user owns could have its ACL changed back by them at any time.
func checkTree(dir, user string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		// WalkDir does not follow links or junctions; they show as such here.
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return fmt.Errorf("unsafe data directory: %s is a link or junction (inspect it and remove it)", path)
		}
		h, err := openNoFollow(path, windows.READ_CONTROL)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		var fi windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
			return err
		}
		if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("unsafe data directory: %s is a reparse point (inspect it and remove it)", path)
		}
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the ACL of %s: %w", path, err)
		}
		disk := fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && isVMDisk(path)
		return trustedSD(sd, path, disk, user)
	})
}

// trusted reports whether sid is SYSTEM, Administrators or user.
func trusted(sid, user string) bool {
	return sid == SYSTEM || sid == Administrators || (user != "" && sid == user)
}

func trustedOwner(sd *windows.SECURITY_DESCRIPTOR, path, user string) error {
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if o := owner.String(); !trusted(o, user) {
		want := "SYSTEM or Administrators"
		if user != "" {
			want = "you, SYSTEM or Administrators"
		}
		return fmt.Errorf("unsafe data directory: %s is owned by %s, not %s", path, o, want)
	}
	return nil
}

// VMWorkerCapability is the capability SID HcsGrantVmAccess grants
// alongside a VM's own SID (S-1-5-83-1-...): Hyper-V's VM worker processes
// run with it.
const VMWorkerCapability = "S-1-15-3-1024-2268835264-3721307629-241982045-173645152-1490879176-104643441-2915960892-1612460704"

// isVMDisk reports whether path names a virtual disk, the only kind of file
// glovd lets HcsGrantVmAccess open to VMs.
func isVMDisk(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".vhdx" || ext == ".vhd"
}

// vmACE reports whether an ACE is one HcsGrantVmAccess adds: explicit (not
// inherited, not inheritable), for a VM's SID or the VM worker capability.
func vmACE(ace *windows.ACCESS_ALLOWED_ACE, sid string) bool {
	if ace.Header.AceFlags&(windows.INHERITED_ACE|windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != 0 {
		return false
	}
	return strings.HasPrefix(sid, "S-1-5-83-1-") || sid == VMWorkerCapability
}

// trustedSD checks an owner and DACL: owned by SYSTEM, Administrators or
// user, and no allow ACE for anyone else, except that on a VM disk
// (vmDisk) the ACEs HcsGrantVmAccess adds let the VM open it. Deny ACEs
// only narrow access.
func trustedSD(sd *windows.SECURITY_DESCRIPTOR, path string, vmDisk bool, user string) error {
	if err := trustedOwner(sd, path, user); err != nil {
		return fmt.Errorf("%w (run glov host install again)", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return fmt.Errorf("unsafe data directory: %s has no DACL, which grants everyone access (run glov host install again)", path)
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if !trusted(sid, user) && !(vmDisk && vmACE(ace, sid)) {
			return fmt.Errorf("unsafe data directory: %s grants access to %s (run glov host install again)", path, sid)
		}
	}
	return nil
}

// openNoFollow opens a file or directory itself, never the target of a
// reparse point at that path.
func openNoFollow(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	return h, nil
}

// plainDir fails unless h is a directory that is not a reparse point.
func plainDir(h windows.Handle, path string) error {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return err
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("unsafe data directory %s: a reparse point (junction or link)", path)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("unsafe data directory %s: not a directory", path)
	}
	return nil
}

var procImpersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")

type fder interface{ Fd() uintptr }

// AsPipeClient runs f on the current OS thread while impersonating the
// process at the other end of a server pipe connection. With a client
// connected at identification level, f can query the thread token but not
// open files as the client; that takes impersonation level.
func AsPipeClient(conn net.Conn, f func() error) error {
	h, ok := conn.(fder)
	if !ok {
		return fmt.Errorf("%T has no pipe handle", conn)
	}
	// Impersonation applies to the OS thread: keep this goroutine on it, and
	// if reverting fails, leave the thread locked so Go discards it on exit
	// rather than reuse a thread that still carries the client's identity.
	runtime.LockOSThread()
	if r, _, err := procImpersonateNamedPipeClient.Call(h.Fd()); r == 0 {
		runtime.UnlockOSThread()
		return fmt.Errorf("ImpersonateNamedPipeClient: %w", err)
	}
	ferr := f()
	if rerr := windows.RevertToSelf(); rerr != nil {
		return fmt.Errorf("RevertToSelf: %w", rerr)
	}
	runtime.UnlockOSThread()
	return ferr
}

// PipeClient returns the SID and the Remote Desktop session ID (not the
// logon session) of the process at the other end of a server pipe connection,
// read from its impersonation token. The client must connect at
// identification level or above.
func PipeClient(conn net.Conn) (sid string, session uint32, err error) {
	err = AsPipeClient(conn, func() error {
		var tok windows.Token
		if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok); err != nil {
			return fmt.Errorf("OpenThreadToken: %w", err)
		}
		defer tok.Close()
		u, err := tok.GetTokenUser()
		if err != nil {
			return err
		}
		sid = u.User.Sid.String()
		var n uint32
		return windows.GetTokenInformation(tok, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), uint32(unsafe.Sizeof(session)), &n)
	})
	return sid, session, err
}

// PipeClientSID returns the SID of the process at the other end of a server
// pipe connection (see PipeClient).
func PipeClientSID(conn net.Conn) (string, error) {
	sid, _, err := PipeClient(conn)
	return sid, err
}

// PipeOwner returns the owner SID of the pipe a client connection reaches.
// glovd creates its pipe owned by SYSTEM, which another user cannot do,
// so a client that checks it knows it is not talking to an impostor.
func PipeOwner(conn net.Conn) (string, error) {
	f, ok := conn.(fder)
	if !ok {
		return "", fmt.Errorf("%T has no pipe handle", conn)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("read the pipe owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", err
	}
	return owner.String(), nil
}

// CurrentSID returns the SID of the user this process runs as.
func CurrentSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// Elevated reports whether this process runs with a full administrator token.
func Elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
