package vmrun

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/windows/internal/api"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// Usage measures the running VM; it fails only when HCS does not answer. A
// VM holds its configured memory size in RAM.
func (m *Manager) Usage(id string) (Usage, error) {
	r, ok := m.get(id)
	if !ok {
		return Usage{}, ErrNotRunning
	}
	st, err := r.sys.Stats()
	if err != nil {
		return Usage{}, err
	}
	u := Usage{CPUTime: time.Duration(st.CPUTime100ns) * 100}
	size := uint64(r.spec.MemoryMiB) << 20
	u.Resident, u.Committed = &size, &size
	return u, nil
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length, MemoryLoad                       uint32
	TotalPhys, AvailPhys                     uint64
	TotalPageFile, AvailPageFile             uint64
	TotalVirtual, AvailVirtual, AvailExtVirt uint64
}

// HostMemory is the host's physical memory and commit charge.
func HostMemory() (api.HostMemory, error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return api.HostMemory{}, err
	}
	return api.HostMemory{Physical: m.TotalPhys, Available: m.AvailPhys,
		CommitUsed: m.TotalPageFile - m.AvailPageFile, CommitLimit: m.TotalPageFile}, nil
}
