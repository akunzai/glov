package guestservice

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/akunzai/glov/guest/internal/exec"
	"github.com/akunzai/glov/guest/internal/winsys"
)

var procSetSystemTime = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetSystemTime")

func hostInfo() (hostname string, user *string, bootID string) {
	hostname, _ = os.Hostname()
	return hostname, consoleUser(), bootTime()
}

// consoleUser is the account name of the user signed in on the console.
func consoleUser() *string {
	tok, err := exec.ConsoleUserToken()
	if err != nil {
		return nil
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil
	}
	name, _, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return nil
	}
	return &name
}

// bootTime identifies the current boot by the creation time of the System
// process, which is stored absolutely and so does not move when time.sync
// steps the clock. Without access to it, fall back to the kernel's boot time.
func bootTime() string {
	if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, 4); err == nil {
		defer windows.CloseHandle(h)
		var creation, exit, kernel, usr windows.Filetime
		if windows.GetProcessTimes(h, &creation, &exit, &kernel, &usr) == nil {
			return time.Unix(0, creation.Nanoseconds()).UTC().Format(time.RFC3339Nano)
		}
	}
	var info struct {
		BootTime, CurrentTime, TimeZoneBias int64
		TimeZoneID                          uint32
		_                                   uint32
		BootTimeBias, SleepTimeBias         uint64
	}
	var n uint32
	if windows.NtQuerySystemInformation(windows.SystemTimeOfDayInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &n) == nil {
		ft := windows.Filetime{LowDateTime: uint32(info.BootTime), HighDateTime: uint32(info.BootTime >> 32)}
		return time.Unix(0, ft.Nanoseconds()).UTC().Format(time.RFC3339Nano)
	}
	return "unknown"
}

func setSystemTime(t time.Time) error {
	if err := winsys.EnablePrivilege("SeSystemtimePrivilege"); err != nil {
		return err
	}
	t = t.UTC()
	st := windows.Systemtime{
		Year: uint16(t.Year()), Month: uint16(t.Month()), DayOfWeek: uint16(t.Weekday()), Day: uint16(t.Day()),
		Hour: uint16(t.Hour()), Minute: uint16(t.Minute()), Second: uint16(t.Second()), Milliseconds: uint16(t.Nanosecond() / 1e6),
	}
	if r, _, err := procSetSystemTime.Call(uintptr(unsafe.Pointer(&st))); r == 0 {
		return err
	}
	return nil
}

// prepareReboot checks the privilege now so a failure can still be reported in
// the reply; the returned func restarts Windows after the reply is sent. A
// restart, unlike a shutdown, never takes the Fast Startup path. Apps that hang
// are forced closed; others get the normal shutdown path.
var prepareReboot = func() (func() error, error) {
	if err := winsys.EnablePrivilege("SeShutdownPrivilege"); err != nil {
		return nil, err
	}
	flags := uint32(windows.EWX_REBOOT | windows.EWX_FORCEIFHUNG)
	return func() error { return windows.ExitWindowsEx(flags, 0x80000000|0x00050000) }, nil // planned, other
}
