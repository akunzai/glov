package vmrun

import (
	"errors"
	"time"
)

// StopTimeout bounds a graceful stop before the VM is turned off.
const StopTimeout = 60 * time.Second

// ErrNotRunning is returned for a VM that is not running.
var ErrNotRunning = errors.New("the VM is not running")

// ExecParams are the Guest Service's exec.start parameters.
type ExecParams struct {
	Argv []string          `json:"argv"`
	Cwd  string            `json:"cwd,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	As   string            `json:"as,omitempty"`
}

// Usage is what a running VM holds on the host.
type Usage struct {
	// CPUTime is the processor time of all its vCPUs since it started.
	CPUTime time.Duration
	// Resident and Committed are its memory in host RAM and committed for
	// it, in bytes.
	Resident, Committed *uint64
}

// FileMethods are the Guest Service methods GuestCall passes on: what glov
// cp needs. The VM's owner runs anything as root through exec already, so
// these grant nothing more.
var FileMethods = map[string]bool{"file.stat": true, "file.list": true, "file.read": true, "file.write": true}

// SyncMethods are the Guest Service's share sync methods, which glovd
// calls for glov share sync and live shares.
var SyncMethods = map[string]bool{"sync.manifest": true, "sync.apply": true, "sync.status": true, "sync.cancel": true,
	"sync.watch": true, "sync.read": true, "sync.unwatch": true}
