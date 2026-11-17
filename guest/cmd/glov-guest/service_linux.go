package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"syscall"
)

const (
	serviceName = "glov-guest"
	installDir  = "/usr/local/bin"
	unitDir     = "/etc/systemd/system"
)

// unit starts after the module loader so vsock is probably there; the Guest Service
// retries listening anyway. Type=exec reports executable startup failures.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#Type=
// StartLimitIntervalSec=0 keeps systemd from giving
// up on a crash loop, since the host depends on the Guest Service coming back.
// A Guest Service that hangs past TimeoutStopSec gets SIGABRT, whose goroutine
// dump lands in the journal: the only trace of where it hung.
func unit(bin string) string {
	return `[Unit]
Description=glov Guest Service
After=systemd-modules-load.service local-fs.target
StartLimitIntervalSec=0

[Service]
Type=exec
User=root
ExecStart=` + bin + `
Restart=always
RestartSec=2
TimeoutStopSec=10
TimeoutStopFailureMode=abort

[Install]
WantedBy=multi-user.target
`
}

// installer carries the paths and side effects of install and uninstall so
// that tests can point them at a temporary root.
type installer struct {
	binDir, unitDir string
	self            string // the running binary
	ctl             func(args ...string) error
	active          func() bool
	checkOwner      bool // require root-owned, non-world-writable install paths
}

func newInstaller() (*installer, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("must run as root")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &installer{
		binDir: installDir, unitDir: unitDir, self: self, checkOwner: true,
		ctl: func(args ...string) error {
			out, err := osexec.Command("systemctl", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
			}
			return nil
		},
		active: func() bool { return osexec.Command("systemctl", "is-active", "--quiet", serviceName).Run() == nil },
	}, nil
}

func (in *installer) bin() string  { return filepath.Join(in.binDir, serviceName) }
func (in *installer) unit() string { return filepath.Join(in.unitDir, serviceName+".service") }

// rootOnly fails unless path is owned by root and not writable by anyone else:
// the service runs the binary as root, so a user who can replace it owns the VM.
func (in *installer) rootOnly(path string) error {
	if !in.checkOwner {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be owned by root and not writable by group or others", path)
	}
	return nil
}

// install copies the binary to /usr/local/bin (unless it already runs from
// there), writes the unit, and enables and starts the service. A running
// service is restarted so a new binary takes effect.
func (in *installer) install() error {
	if err := in.rootOnly(in.binDir); err != nil {
		return err
	}
	wasActive := in.active()
	if err := in.copySelf(); err != nil {
		return err
	}
	if err := in.rootOnly(in.bin()); err != nil {
		return err
	}
	if err := os.WriteFile(in.unit(), []byte(unit(in.bin())), 0o644); err != nil {
		return err
	}
	if err := in.ctl("daemon-reload"); err != nil {
		return err
	}
	if err := in.ctl("enable", "--now", serviceName+".service"); err != nil {
		return err
	}
	if wasActive {
		if err := in.ctl("restart", serviceName+".service"); err != nil {
			return err
		}
	}
	fmt.Println("service", serviceName, "installed and started")
	return nil
}

func (in *installer) copySelf() error {
	dst := in.bin()
	if a, err := os.Stat(in.self); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	src, err := os.Open(in.self)
	if err != nil {
		return err
	}
	defer src.Close()
	// Write beside the target and rename, so a running Guest Service keeps its old
	// inode and nobody ever executes a half-written file.
	tmp, err := os.CreateTemp(in.binDir, ".glov-guest.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// uninstall stops and disables the service and removes the unit and the
// installed binary.
func (in *installer) uninstall() error {
	if _, err := os.Stat(in.unit()); err != nil {
		return fmt.Errorf("service %s is not installed: %w", serviceName, err)
	}
	if err := in.ctl("disable", "--now", serviceName+".service"); err != nil {
		return err
	}
	for _, p := range []string{in.unit(), in.bin()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := in.ctl("daemon-reload"); err != nil {
		return err
	}
	fmt.Println("service", serviceName, "removed")
	return nil
}

func install() error {
	in, err := newInstaller()
	if err != nil {
		return err
	}
	return in.install()
}

func uninstall() error {
	in, err := newInstaller()
	if err != nil {
		return err
	}
	return in.uninstall()
}
