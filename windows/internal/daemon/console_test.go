package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/vmrun"
)

func TestConsoleReadsTheLogAndNoticesANewBoot(t *testing.T) {
	f, call := lifecycle(t)
	err := call(api.MethodVMConsole, api.ConsoleParams{VM: "dev"}, nil)
	if err == nil || !strings.Contains(err.Error(), "has not started") {
		t.Fatalf("console before a boot err = %v", err)
	}
	path := filepath.Join(f.st.Dir("dev"), vmrun.ConsoleLog)
	writeLog := func(data string, created time.Time) { t.Helper(); writeBootLog(t, path, data, created) }
	boot := time.Now()
	writeLog("first boot\x1b[0m\n", boot)

	var r api.ConsoleRead
	if err := call(api.MethodVMConsole, api.ConsoleParams{VM: "dev", MaxBytes: 5}, &r); err != nil {
		t.Fatal(err)
	}
	if string(r.Data) != "first" || r.Offset != 5 || r.Log == "" || r.Restarted || r.Running {
		t.Fatalf("first read = %+v", r)
	}
	log := r.Log
	if err := call(api.MethodVMConsole, api.ConsoleParams{VM: "dev", Log: log, Offset: r.Offset}, &r); err != nil {
		t.Fatal(err)
	}
	if string(r.Data) != " boot\x1b[0m\n" || r.Log != log || r.Restarted {
		t.Fatalf("second read = %+v", r)
	}
	if err := call(api.MethodVMConsole, api.ConsoleParams{VM: "dev", Log: log, Offset: r.Offset}, &r); err != nil || len(r.Data) != 0 {
		t.Fatalf("read at the end = %+v, %v", r, err)
	}

	// The next boot's log is longer than the offset, but another log.
	writeLog("second boot, longer than the first\n", boot.Add(time.Second))
	if err := call(api.MethodVMConsole, api.ConsoleParams{VM: "dev", Log: log, Offset: 5}, &r); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(r.Data), "second boot") || !r.Restarted || r.Log == log {
		t.Fatalf("read after a new boot = %+v", r)
	}

	if err := call(api.MethodVMConsole, api.ConsoleParams{VM: "other"}, nil); err == nil {
		t.Fatal("read another user's VM's console")
	}
}
