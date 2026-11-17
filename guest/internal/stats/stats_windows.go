package stats

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
)

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	length, memoryLoad                   uint32
	totalPhys, availPhys                 uint64
	totalPageFile, availPageFile         uint64
	totalVirtual, availVirtual, availExt uint64
}

func collect(interval time.Duration) (Stats, error) {
	before, err := cpu()
	if err != nil {
		return Stats{}, err
	}
	m := memoryStatusEx{length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return Stats{}, err
	}
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	s := Stats{MemoryTotal: m.totalPhys, MemoryAvailable: m.availPhys, DiskPath: drive + `\`}
	path, err := windows.UTF16PtrFromString(s.DiskPath)
	if err != nil {
		return Stats{}, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(path, &available, &total, &free); err != nil {
		return Stats{}, err
	}
	s.DiskTotal, s.DiskUsed, s.DiskAvailable = total, total-free, available
	time.Sleep(interval)
	after, err := cpu()
	if err != nil {
		return Stats{}, err
	}
	s.CPUBusy = busy(before, after)
	return s, nil
}

// cpu reads GetSystemTimes, whose kernel time includes the idle time.
func cpu() (cpuTimes, error) {
	var idle, kernel, user windows.Filetime
	if r, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return cpuTimes{}, err
	}
	ticks := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	total := ticks(kernel) + ticks(user)
	return cpuTimes{busy: total - ticks(idle), total: total}, nil
}
