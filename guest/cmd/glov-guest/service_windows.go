package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "GlovGuestService"
	logPath     = `C:\ProgramData\glov\guest-service.log`
)

func runsAsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

type service struct{ run func(context.Context) }

func (s service) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case <-done:
			return false, 0
		case r := <-requests:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		}
	}
}

func runService(run func(context.Context)) error { return svc.Run(serviceName, service{run}) }

// updatedConfig is the configuration to apply to an installed service: the
// current one with the fields install owns refreshed. ChangeServiceConfig
// rejects a zero ServiceType with ERROR_INVALID_PARAMETER, so the update must
// start from what the service has, not from an empty Config.
func updatedConfig(cur mgr.Config) mgr.Config {
	cur.DisplayName = "glov Guest Service"
	cur.Description = serviceDescription
	cur.StartType = mgr.StartAutomatic
	return cur
}

const serviceDescription = "Lets the glov host run commands and move files in this VM."

// binaryPath extracts the executable from a service's BinaryPathName.
func binaryPath(cmdline string) string {
	if strings.HasPrefix(cmdline, `"`) {
		if i := strings.Index(cmdline[1:], `"`); i >= 0 {
			return cmdline[1 : 1+i]
		}
	}
	return cmdline
}

// install registers the service and starts it. On an installed Guest Service it is the
// update path: the running Guest Service's binary is replaced with this one, between
// stopping and starting the service, and a failure puts the old binary back and
// starts the Guest Service again. Stopping the Guest Service ends any exec running through it,
// so run an update from a one-shot SYSTEM scheduled task (docs/agents/verification.md).
func install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err == nil {
		defer s.Close()
		cur, err := s.Config()
		if err != nil {
			return fmt.Errorf("read service config: %w", err)
		}
		if err := s.UpdateConfig(updatedConfig(cur)); err != nil {
			return fmt.Errorf("update service: %w", err)
		}
		if dst := binaryPath(cur.BinaryPathName); !sameFile(exe, dst) {
			if err := swapBinary(exe, dst, func() error { return stopService(s) }, func() error { return startService(s) }); err != nil {
				return fmt.Errorf("update Guest Service: %w", err)
			}
		}
	} else {
		if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return err
		}
		cfg := mgr.Config{
			DisplayName: "glov Guest Service",
			Description: serviceDescription,
			StartType:   mgr.StartAutomatic, // account left empty: LocalSystem
		}
		if s, err = m.CreateService(serviceName, exe, cfg); err != nil {
			return fmt.Errorf("create service: %w", err)
		}
		defer s.Close()
	}
	recovery := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}
	if err := s.SetRecoveryActions(recovery, 86400); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if st, err := s.Query(); err == nil && st.State == svc.Running {
		fmt.Println("service", serviceName, "installed and already running")
		return nil
	}
	if err := startService(s); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Println("service", serviceName, "installed and started")
	return nil
}

func sameFile(a, b string) bool {
	x, err := os.Stat(a)
	if err != nil {
		return false
	}
	y, err := os.Stat(b)
	return err == nil && os.SameFile(x, y)
}

// StartService can return while startup is pending. Wait for Running so a failed
// replacement takes the normal binary-swap rollback path.
// https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-startservicew
func startService(s *mgr.Service) error {
	if err := s.Start(); err != nil {
		return err
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		state, err := s.Query()
		if err != nil {
			return err
		}
		if state.State == svc.Running {
			return nil
		}
		if state.State == svc.Stopped {
			return errors.New("Guest Service stopped during startup")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("Guest Service did not start in 15 s")
}

// stopService stops the service if it runs and waits until it has stopped, so
// its executable is released.
func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if st, err = s.Control(svc.Stop); err != nil {
			return err
		}
	}
	for deadline := time.Now().Add(15 * time.Second); st.State != svc.Stopped; {
		if time.Now().After(deadline) {
			return errors.New("service did not stop in 15 s")
		}
		time.Sleep(200 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

// uninstall stops the service if it runs and removes it.
func uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed: %w", serviceName, err)
	}
	defer s.Close()
	stopService(s) // best effort: Delete marks a running service for removal
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	fmt.Println("service", serviceName, "removed")
	return nil
}
