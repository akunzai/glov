// Package broker is glov-broker, the small LocalSystem service that makes
// the Host Compute Service calls a non-elevated glovd cannot, and glovd's
// client for it.
//
// The broker builds each VM's HCS document itself from a narrow spec, owns
// the VM IDs (each tied to the SID of the user it was handed to) and only
// lets a user drive VMs whose IDs it handed that user. It never grants
// files to VMs: glovd grants its own disks with the user's token, and the
// VM worker opens them as the VM's own account, so a disk the user could
// not grant does not open.
//
// The wire format is glov's framed JSON-RPC 2.0 (docs/guest-protocol.md)
// on PipeName.
package broker

// PipeName is the broker's pipe. LocalSystem owns it, which glovd checks.
const PipeName = `\\.\pipe\glov-broker`

// Protocol is the broker protocol this build speaks. A broker answers
// any client; a client refuses a broker older than MinProtocol, and a
// feature that needs a newer broker checks Hello.Protocol and names
// `glov host install-broker`.
const (
	Protocol    = 1
	MinProtocol = 1
)

// Methods. Every method but hello and id.new names a VM ID, which must be
// one the broker handed the calling user.
const (
	// MethodHello exchanges protocol versions: Hello in, Hello out.
	MethodHello = "hello"
	// MethodNewID hands the caller a new VM ID: nothing in, IDParams out.
	MethodNewID = "id.new"
	// MethodReleaseID forgets a stopped VM's ID once its VM is deleted.
	MethodReleaseID = "id.release"
	// MethodCreate creates a VM without starting it: CreateParams in. Its
	// disks stay pinned until vm.start, which is when HCS opens them.
	MethodCreate = "vm.create"
	// MethodStart starts a created VM: IDParams in. glovd binds the VM's
	// hv_sock services in between, before the guest boots.
	MethodStart = "vm.start"
	// MethodState reports a VM's state: IDParams in, State out.
	MethodState = "vm.state"
	// MethodShutdown asks the guest to power off; it returns once the
	// guest has taken the request.
	MethodShutdown = "vm.shutdown"
	// MethodTerminate turns a VM off and lets the broker forget its handle.
	MethodTerminate = "vm.terminate"
	// MethodStats measures a running VM: IDParams in, Stats out.
	MethodStats = "vm.stats"
)

// CodeAlreadyRunning is vm.create's error for a VM that runs already, as
// after glovd restarted.
const CodeAlreadyRunning = -32010

// Hello is hello's parameters and result: the protocol each side speaks.
type Hello struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version,omitempty"`
}

// IDParams names a VM.
type IDParams struct {
	ID string `json:"id"`
}

// CreateParams is vm.create's spec. Disk and Seed must be virtual disks the
// caller owns, under the VM directory root of the caller's glov home (see
// VMRoot); the broker opens them as the caller and refuses links and
// junctions on the way.
type CreateParams struct {
	ID        string `json:"id"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memoryMiB"`
	// Disk is the VM's VHDX, which UEFI boots.
	Disk string `json:"disk"`
	// Seed, if set, is a NoCloud seed disk attached read-only.
	Seed       string `json:"seed,omitempty"`
	SecureBoot bool   `json:"secureBoot,omitempty"`
	// KeepRunning keeps the VM running when the connection that started it
	// ends, as when glovd restarts; without it the broker turns the VM off
	// then, as for a provisioning boot.
	KeepRunning bool `json:"keepRunning,omitempty"`
}

// State is a VM's state as HCS reports it, such as "Running"; empty when
// HCS has no system with that ID.
type State struct {
	State string `json:"state"`
}

// Running is the state of a running VM.
const Running = "Running"

// Stats measures a running VM.
type Stats struct {
	// CPUTime100ns is the processor time the VM used, in 100 ns units.
	CPUTime100ns uint64 `json:"cpuTime100ns"`
}

// MaxIDs is how many VM IDs glov-broker hands one user.
const MaxIDs = 1000

// OwnerPrefix starts the HCS owner of every VM glov-broker creates,
// followed by the SID of the user it belongs to.
const OwnerPrefix = "glov "

// ConsolePipe is the named pipe a VM's COM1 connects to. glovd creates it,
// granting the VM's account, before vm.start; the broker names it in the
// VM's document.
func ConsolePipe(id string) string { return `\\.\pipe\glov-console-` + id }
