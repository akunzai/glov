package service

import (
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/store"
)

func TestParseFeatureState(t *testing.T) {
	out := "Deployment Image Servicing and Management tool\r\nVersion: 10.0.26300.1\r\n\r\nFeature Information:\r\n\r\nFeature Name : VirtualMachinePlatform\r\nDisplay Name : Virtual Machine Platform\r\nRestart Required : Possible\r\nState : Enable Pending\r\n"
	if got, err := parseFeatureState(out); err != nil || got != "Enable Pending" {
		t.Fatalf("parseFeatureState = %q, %v", got, err)
	}
	if _, err := parseFeatureState("Error: 87\r\n"); err == nil {
		t.Fatal("parseFeatureState accepted output without a state")
	}
}

func TestCommandLineQuotesTheHomePath(t *testing.T) {
	got := commandLine(`C:\Users\A B\AppData\Local\glov\bin\glovd.exe`, "")
	if got != `"C:\Users\A B\AppData\Local\glov\bin\glovd.exe"` {
		t.Fatalf("commandLine = %s", got)
	}
}

// glovd inherited the shell's working directory and kept it in use: the
// unpacked release glov host install ran from could not be deleted.
func TestGlovdRunsInItsOwnFolder(t *testing.T) {
	line := commandLine(`C:\Users\A B\AppData\Local\glov\bin\glovd.exe`, `D:\glov cache`)
	cmd, err := glovdCommand(line)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != `C:\Users\A B\AppData\Local\glov\bin` {
		t.Errorf("Dir = %q", cmd.Dir)
	}
	if strings.Join(cmd.Args[1:], " ") != `--cache D:\glov cache` {
		t.Errorf("Args = %q", cmd.Args)
	}
}

func TestCacheDirSurvivesTheCommandLine(t *testing.T) {
	for _, cache := range []string{`D:\glov cache`, `E:\c`, ""} {
		line := commandLine(`C:\Users\a\AppData\Local\glov\bin\glovd.exe`, cache)
		if got := cacheFlag(line); got != cache {
			t.Errorf("cacheFlag(%s) = %q, want %q", line, got, cache)
		}
	}
}

func TestPathHasIgnoresCaseAndTrailingSlashes(t *testing.T) {
	list := `C:\Windows;c:\users\a\appdata\local\glov\BIN\;;D:\tools`
	if !pathHas(list, `C:\Users\a\AppData\Local\glov\bin`) {
		t.Error("pathHas missed the bin folder")
	}
	if pathHas(list, `C:\Users\a\AppData\Local\glov`) || pathHas(list, ``) {
		t.Error("pathHas matched another folder")
	}
}

func TestCachePathMustBeLocalAndAbsolute(t *testing.T) {
	for _, dir := range []string{`D:\glov\cache`, `C:\x`} {
		if err := checkCachePath(dir); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}
	for _, dir := range []string{`cache`, `\cache`, `D:cache`, `\\server\share\cache`, `\\?\D:\cache`} {
		if err := checkCachePath(dir); err == nil {
			t.Errorf("%s accepted", dir)
		}
	}
}

func TestCacheCannotMoveAwayFromVMsBaseImages(t *testing.T) {
	data := t.TempDir()
	st := store.Store{Root: data}
	st.Create(store.Record{Name: "dev", Owner: "S-1-5-21-1-2-3-1001", BaseDisk: `C:\Users\a\AppData\Local\glov\cache\debian-r0\base.vhdx`})
	st.Create(store.Record{Name: "web", Owner: "S-1-5-21-1-2-3-1001", BaseDisk: `C:\Users\a\AppData\Local\glov\cache\debian-r1\base.vhdx`})
	for _, ok := range []string{`C:\Users\a\AppData\Local\glov\cache`, `c:\users\a\appdata\local\glov\CACHE\`} {
		if err := checkVMsInCache(data, ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	err := checkVMsInCache(data, `D:\cache`)
	if err == nil || !strings.Contains(err.Error(), "dev (") || !strings.Contains(err.Error(), "web (") {
		t.Fatalf("moving the cache err = %v, want both VMs named", err)
	}
	if err := checkVMsInCache(t.TempDir(), `D:\cache`); err != nil {
		t.Fatalf("no VMs: %v", err)
	}
}

func TestInDir(t *testing.T) {
	for path, want := range map[string]bool{
		`C:\c\r\base.vhdx`: true,
		`C:\C\R\BASE.VHDX`: true,
		`C:\c`:             false,
		`C:\cache2\r\b`:    false,
		`C:\c\..\other\b`:  false,
		`D:\c\r\base.vhdx`: false,
		``:                 false,
	} {
		if got := inDir(path, `C:\c`); got != want {
			t.Errorf("inDir(%q) = %v", path, got)
		}
	}
}

func TestPreShutdownTimeoutOutlastsTheGuestsShutdown(t *testing.T) {
	if PreShutdownTimeout < broker.DefaultStopTimeout+20*time.Second {
		t.Errorf("PreShutdownTimeout %s leaves too little room after broker.DefaultStopTimeout %s to turn hung VMs off", PreShutdownTimeout, broker.DefaultStopTimeout)
	}
}
