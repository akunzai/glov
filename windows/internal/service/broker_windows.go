package service

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/akunzai/glov/windows/internal/broker"
	"github.com/akunzai/glov/windows/internal/hcs"
	"github.com/akunzai/glov/windows/internal/winsec"
)

// BrokerName is glov-broker's service name, also its event log source.
const BrokerName = "glov-broker"

// LegacyName is the LocalSystem glovd service earlier builds installed,
// which install-broker removes.
const LegacyName = "glovd"

func knownFolder(id *windows.KNOWNFOLDERID, fallbackEnv string) string {
	if p, err := windows.KnownFolderPath(id, 0); err == nil {
		return p
	}
	return os.Getenv(fallbackEnv)
}

// programFilesGlov is %ProgramFiles%\glov: the broker's folder lies in it,
// as the legacy glovd's binaries did.
func programFilesGlov() string {
	return filepath.Join(knownFolder(windows.FOLDERID_ProgramFiles, "ProgramFiles"), "glov")
}

// BrokerInstallDir holds glov-broker.exe; only administrators can write
// it, so the LocalSystem service never runs a file a user could replace.
func BrokerInstallDir() string { return filepath.Join(programFilesGlov(), "broker") }

// BrokerDataDir holds the broker's VM ID owners and its log; only SYSTEM
// and administrators can open it.
func BrokerDataDir() string {
	return filepath.Join(knownFolder(windows.FOLDERID_ProgramData, "ProgramData"), "glov-broker")
}

// legacyDataDir is where the legacy glovd kept its VMs.
func legacyDataDir() string {
	return filepath.Join(knownFolder(windows.FOLDERID_ProgramData, "ProgramData"), "glov")
}

// InstallBroker, run elevated, enables VirtualMachinePlatform if needed,
// removes the legacy LocalSystem glovd, and installs or upgrades
// glov-broker from srcDir and starts it. VMs keep running across an
// upgrade.
func InstallBroker(srcDir string, out io.Writer) error {
	if !winsec.Elevated() {
		return errors.New("glov host install-broker needs an elevated shell (Run as administrator), once")
	}
	if err := checkPlatform(out); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	if err := removeLegacy(m, out); err != nil {
		return err
	}
	s, err := m.OpenService(BrokerName)
	existing := err == nil
	if existing {
		defer s.Close()
		if err := stop(s); err != nil {
			return err
		}
	}
	if err := copyBinaries(srcDir, BrokerInstallDir(), []string{"glov-broker.exe"}); err != nil {
		if existing {
			if serr := s.Start(); serr != nil {
				return fmt.Errorf("%w; glov-broker did not start again either: %v", err, serr)
			}
			return fmt.Errorf("%w; glov-broker is running again, not upgraded", err)
		}
		return err
	}
	if err := winsec.CreateDataDir(BrokerDataDir(), ""); err != nil {
		return err
	}
	// Exists after a previous install; anything else shows when it is used.
	_ = eventlog.InstallAsEventCreate(BrokerName, eventlog.Error|eventlog.Warning|eventlog.Info)

	exe := filepath.Join(BrokerInstallDir(), "glov-broker.exe")
	if existing {
		cfg, err := s.Config()
		if err != nil {
			return err
		}
		cfg.BinaryPathName = windows.EscapeArg(exe)
		cfg.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("update the glov-broker service: %w", err)
		}
	} else {
		s, err = m.CreateService(BrokerName, exe, mgr.Config{
			DisplayName: "glov broker",
			Description: "Starts and stops glov's Linux VMs for each user's glovd (glov).",
			StartType:   mgr.StartAutomatic,
		})
		if err != nil {
			return fmt.Errorf("create the glov-broker service: %w", err)
		}
		defer s.Close()
	}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("set glov-broker's recovery actions: %w", err)
	}
	if err := setPreShutdownTimeout(s, PreShutdownTimeout); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start glov-broker: %w", err)
	}
	if err := waitState(s, svc.Running); err != nil {
		return fmt.Errorf("%w; see the Application event log, source %s", err, BrokerName)
	}
	fmt.Fprintf(out, "glov-broker installed in %s and running; now run glov host install from a non-elevated shell\n", BrokerInstallDir())
	return nil
}

// removeLegacy stops and deletes the LocalSystem glovd service earlier
// builds installed, and its binaries. Its data stays.
func removeLegacy(m *mgr.Mgr, out io.Writer) error {
	s, err := m.OpenService(LegacyName)
	if err != nil {
		return nil
	}
	defer s.Close()
	if err := stop(s); err != nil {
		return fmt.Errorf("the old glovd service: %w", err)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete the old glovd service: %w", err)
	}
	_ = eventlog.Remove(LegacyName)
	dir := programFilesGlov()
	for _, b := range Binaries {
		olds, _ := filepath.Glob(filepath.Join(dir, b) + ".old*")
		for _, p := range append(olds, filepath.Join(dir, b)) {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				removeAtRestart(p)
			}
		}
	}
	fmt.Fprintf(out, "removed the old LocalSystem glovd service; its VMs and data stay in %s: recreate the VMs, then delete that folder\n", legacyDataDir())
	return nil
}

// UninstallBroker, run elevated, stops and removes glov-broker. It refuses
// while a glov VM runs: without the broker nobody could stop it.
func UninstallBroker(out io.Writer) error {
	if !winsec.Elevated() {
		return errors.New("glov host uninstall-broker needs an elevated shell (Run as administrator)")
	}
	if running, err := runningVMs(); err != nil {
		return err
	} else if running > 0 {
		return fmt.Errorf("%d glov VMs are running: stop them first (glov vm stop, as each user)", running)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(BrokerName); err == nil {
		if err := stop(s); err != nil {
			s.Close()
			return err
		}
		err := s.Delete()
		s.Close()
		if err != nil {
			return fmt.Errorf("delete the glov-broker service: %w", err)
		}
	}
	_ = eventlog.Remove(BrokerName)
	if err := removeInstallDir(BrokerInstallDir()); err != nil {
		return err
	}
	os.Remove(programFilesGlov()) // if empty
	fmt.Fprintf(out, "glov-broker removed; VM IDs stay recorded in %s\n", BrokerDataDir())
	return nil
}

// runningVMs counts running compute systems the broker made.
func runningVMs() (int, error) {
	doc, err := hcs.Enumerate("")
	if err != nil {
		return 0, fmt.Errorf("list compute systems: %w", err)
	}
	var systems []struct{ Owner, State string }
	if err := json.Unmarshal([]byte(doc), &systems); err != nil {
		return 0, fmt.Errorf("compute systems %q: %w", doc, err)
	}
	n := 0
	for _, s := range systems {
		if strings.HasPrefix(s.Owner, broker.OwnerPrefix) && s.State == broker.Running {
			n++
		}
	}
	return n, nil
}

// serviceExists reports whether the service manager has a service called
// name; it needs no elevation.
func serviceExists(name string) bool {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(m)
	n, _ := windows.UTF16PtrFromString(name)
	s, err := windows.OpenService(m, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false
	}
	windows.CloseServiceHandle(s)
	return true
}

// PreShutdownTimeout is how long Windows waits for glov-broker to stop the
// VMs when the host shuts down: the guests' own shutdown
// (broker.DefaultStopTimeout) with room to turn off the ones that hang.
// Windows' default is far shorter.
const PreShutdownTimeout = 90 * time.Second

// setPreShutdownTimeout sets SERVICE_CONFIG_PRESHUTDOWN_INFO, which mgr
// does not wrap.
func setPreShutdownTimeout(s *mgr.Service, d time.Duration) error {
	info := struct{ Timeout uint32 }{uint32(d.Milliseconds())}
	if err := windows.ChangeServiceConfig2(s.Handle, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info))); err != nil {
		return fmt.Errorf("set glov-broker's pre-shutdown timeout: %w", err)
	}
	return nil
}

// checkPlatform makes sure the VirtualMachinePlatform feature, the only one
// glov needs, is enabled, and enables it if not.
func checkPlatform(out io.Writer) error {
	state, err := featureState()
	if err != nil {
		return err
	}
	switch state {
	case "Enabled":
		return nil
	case "Enable Pending":
		fmt.Fprintln(out, "VirtualMachinePlatform is enabled but needs a Windows restart before VMs can start")
		return nil
	}
	fmt.Fprintln(out, "enabling the VirtualMachinePlatform feature...")
	cmd := exec.Command("dism.exe", "/Online", "/Enable-Feature", "/FeatureName:VirtualMachinePlatform", "/All", "/NoRestart", "/English")
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 3010 { // ERROR_SUCCESS_REBOOT_REQUIRED
		err = nil
	}
	if err != nil {
		return fmt.Errorf("enable VirtualMachinePlatform: %w", err)
	}
	fmt.Fprintln(out, "VirtualMachinePlatform enabled: restart Windows before starting VMs")
	return nil
}

// featureState reads VirtualMachinePlatform's state from DISM, in English
// whatever the display language.
func featureState() (string, error) {
	out, err := exec.Command("dism.exe", "/Online", "/Get-FeatureInfo", "/FeatureName:VirtualMachinePlatform", "/English").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("dism /Get-FeatureInfo: %w\n%s", err, out)
	}
	return parseFeatureState(string(out))
}

func parseFeatureState(out string) (string, error) {
	for line := range strings.Lines(out) {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "State" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("dism printed no feature state:\n%s", out)
}

// removeInstallDir deletes dir; a file still in use is removed at the next
// restart.
func removeInstallDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pending := false
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if os.RemoveAll(p) == nil {
			continue
		}
		if err := removeAtRestart(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		pending = true
	}
	if pending {
		return removeAtRestart(dir)
	}
	return os.Remove(dir)
}

func removeAtRestart(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

func stop(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop %s: %w", s.Name, err)
	}
	return waitState(s, svc.Stopped)
}

func waitState(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return fmt.Errorf("%s stopped right after starting", s.Name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still in state %d after 30 s", s.Name, st.State)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
