package logfile

import (
	"fmt"
	"strings"
)

// Rights that let a principal change what is in a directory or who controls it.
const writeRights = 0x2 | // FILE_WRITE_DATA / FILE_ADD_FILE
	0x4 | // FILE_APPEND_DATA / FILE_ADD_SUBDIRECTORY
	0x10 | // FILE_WRITE_EA
	0x40 | // FILE_DELETE_CHILD
	0x100 | // FILE_WRITE_ATTRIBUTES
	0x10000 | // DELETE
	0x40000 | // WRITE_DAC
	0x80000 | // WRITE_OWNER
	0x40000000 | // GENERIC_WRITE
	0x10000000 // GENERIC_ALL

const (
	sidSystem        = "S-1-5-18"
	sidAdministrator = "S-1-5-32-544"
	sidCreatorOwner  = "S-1-3-0"
)

// trusted may own the log directory or hold write rights on it.
func trusted(sid string) bool {
	return sid == sidSystem || sid == sidAdministrator || sid == sidCreatorOwner ||
		strings.HasPrefix(sid, "S-1-5-80-") // service SIDs such as TrustedInstaller
}

// ace is one access-allowed entry of a directory's DACL.
type ace struct {
	SID  string
	Mask uint32
}

// checkPolicy decides whether a pre-existing directory is safe for a
// LocalSystem service to write in: owned by SYSTEM or Administrators, and
// with no allow entry (inherit-only ones included, since new files copy them)
// giving anyone else a write right. A missing or empty DACL grants everyone.
func checkPolicy(owner string, hasDACL bool, aces []ace) error {
	if owner != sidSystem && owner != sidAdministrator {
		return fmt.Errorf("owned by %s, not SYSTEM or Administrators", owner)
	}
	if !hasDACL {
		return fmt.Errorf("no DACL: writable by everyone")
	}
	for _, a := range aces {
		if !trusted(a.SID) && a.Mask&writeRights != 0 {
			return fmt.Errorf("%s may write to it (access mask 0x%x)", a.SID, a.Mask)
		}
	}
	return nil
}
