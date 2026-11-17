package vmrun

import (
	"encoding/json/jsontext"

	"github.com/akunzai/glov/windows/internal/egress"
)

// ConsoleLog is the serial console's log in the VM's directory, written
// anew on every start by glovd.
const ConsoleLog = "console.log"

// Spec is what starting a VM takes.
type Spec struct {
	// ID is the VM's GUID, its HCS ID and hv_sock VM ID.
	ID   string
	Name string
	// Dir is the VM's directory; the serial console is logged there.
	Dir      string
	Disk     string
	BaseDisk string
	// Template, if set, lies between BaseDisk and Disk (a frozen VM).
	Template  string
	CPUs      int
	MemoryMiB int
	// SecureBoot boots with Secure Boot (Debian and Ubuntu).
	SecureBoot bool
	// Egress serves the guest's connections to hv_sock port egress.Port,
	// its only way out: a VM has no network adapter. Start refuses a VM
	// without one (a record from before every VM was isolated).
	Egress *egress.Proxy
	// OnReady, if set, runs in its own goroutine each time a connection to
	// the Guest Service becomes ready (a boot, or a reconnect), as macOS
	// delivers a harness's tmpfs secrets.
	OnReady func()
	// OnNotify, if set, receives the Guest Service's notifications that
	// the VM's session does not handle itself (sync.changed), on the
	// session's read loop: it must not block.
	OnNotify func(method string, params jsontext.Value)
}
