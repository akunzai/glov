//go:build windows

package hcs

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// LinuxVM describes a Linux VM that boots through UEFI from its own disk:
// the image's boot loader and kernel, as on any PC.
type LinuxVM struct {
	// Owner names the VM's owner in HCS, for diagnostics.
	Owner string
	// User is the SID of the user whose glovd talks to the guest: the
	// only account besides SYSTEM that may bind or connect to the VM's
	// hv_sock services.
	User      string
	CPUs      int
	MemoryMiB int
	// Disk is the VM's VHDX, SCSI disk 0, which UEFI boots.
	Disk string
	// Seed, if set, is a NoCloud seed disk attached read-only as SCSI disk 1.
	Seed string
	// Console is the named pipe COM1 connects to; HCS opens it as a client.
	Console string
	// KeepRunning keeps the VM running when the last handle to it closes,
	// such as when glovd stops; it is reopened by its ID.
	KeepRunning bool
	// SecureBoot boots with Secure Boot on, trusting what Microsoft's UEFI
	// CA signed, such as Debian's and Ubuntu's shim.
	SecureBoot bool
}

// MicrosoftUEFICATemplate is Hyper-V's "Microsoft UEFI Certificate
// Authority" Secure Boot template, which Linux shims are signed for.
const MicrosoftUEFICATemplate = "272e7447-90a4-4563-a4b9-8e4ab00526ce"

// hvSocketSDDL lets SYSTEM and user (the owner's glovd) connect to and
// bind the VM's hv_sock services; other users, administrators included,
// cannot reach the guest.
func hvSocketSDDL(user string) string { return "D:P(A;;FA;;;SY)(A;;FA;;;" + user + ")" }

// Document is the HCS compute system document for v.
func (v LinuxVM) Document() map[string]any {
	disks := map[string]any{"0": map[string]any{"Type": "VirtualDisk", "Path": v.Disk}}
	if v.Seed != "" {
		disks["1"] = map[string]any{"Type": "VirtualDisk", "Path": v.Seed, "ReadOnly": true}
	}
	devices := map[string]any{
		"ComPorts": map[string]any{"0": map[string]string{"NamedPipe": v.Console}},
		"HvSocket": map[string]any{"HvSocketConfig": map[string]string{
			"DefaultBindSecurityDescriptor":    hvSocketSDDL(v.User),
			"DefaultConnectSecurityDescriptor": hvSocketSDDL(v.User),
		}},
		"Scsi": map[string]any{"Primary": map[string]any{"Attachments": disks}},
	}
	uefi := map[string]any{
		"ApplySecureBootTemplate": "Skip",
		"Console":                 "ComPort1",
		"BootThis":                map[string]any{"DeviceType": "ScsiDrive", "DiskNumber": 0, "DevicePath": "Primary"},
	}
	if v.SecureBoot {
		uefi["ApplySecureBootTemplate"] = "Apply"
		uefi["SecureBootTemplateId"] = MicrosoftUEFICATemplate
	}
	memory := map[string]any{"SizeInMB": v.MemoryMiB}
	return map[string]any{
		"Owner":                             v.Owner,
		"SchemaVersion":                     map[string]int{"Major": 2, "Minor": 5},
		"ShouldTerminateOnLastHandleClosed": !v.KeepRunning,
		"VirtualMachine": map[string]any{
			"StopOnReset": true,
			// Shutdown lets System.Shutdown power the guest off cleanly.
			"Services": map[string]any{"Shutdown": map[string]any{}, "Heartbeat": map[string]any{}},
			"Chipset": map[string]any{
				"UseUtc": true,
				"Uefi":   uefi,
			},
			"ComputeTopology": map[string]any{
				"Memory":    memory,
				"Processor": map[string]any{"Count": v.CPUs},
			},
			"Devices": devices,
		},
	}
}

// VMSID is the SID of the virtual account a VM's worker process runs as,
// for the VM whose HCS ID is id: S-1-5-83-1 and the GUID's 16 bytes as four
// little-endian numbers. HcsGrantVmAccess grants files to it.
func VMSID(id guid.GUID) string {
	b := id.ToWindowsArray()
	return fmt.Sprintf("S-1-5-83-1-%d-%d-%d-%d",
		binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint32(b[4:]),
		binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:]))
}

// State is the system's state as HCS reports it, such as "Running". A
// stopped system that HCS has already cleaned up reports an error.
func (s *System) State() (string, error) {
	doc, err := s.Properties("")
	if err != nil {
		return "", err
	}
	var p struct{ State string }
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return "", fmt.Errorf("properties %q: %w", doc, err)
	}
	return p.State, nil
}
