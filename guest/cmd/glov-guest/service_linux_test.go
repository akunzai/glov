package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fixture struct {
	in    *installer
	calls [][]string
	root  string
}

func newFixture(t *testing.T, active bool) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{root: root}
	self := filepath.Join(root, "download", "glov-guest")
	os.MkdirAll(filepath.Dir(self), 0o755)
	os.WriteFile(self, []byte("binary-v1"), 0o755)
	f.in = &installer{
		binDir: filepath.Join(root, "bin"), unitDir: filepath.Join(root, "units"), self: self,
		ctl:    func(a ...string) error { f.calls = append(f.calls, a); return nil },
		active: func() bool { return active },
	}
	os.MkdirAll(f.in.binDir, 0o755)
	os.MkdirAll(f.in.unitDir, 0o755)
	return f
}

func (f *fixture) verbs() []string {
	var v []string
	for _, c := range f.calls {
		v = append(v, c[0])
	}
	return v
}

func TestInstallCopiesBinaryWritesUnitAndEnables(t *testing.T) {
	f := newFixture(t, false)
	if err := f.in.install(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.in.bin()); string(b) != "binary-v1" {
		t.Fatalf("installed binary = %q", b)
	}
	if fi, _ := os.Stat(f.in.bin()); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	u, _ := os.ReadFile(f.in.unit())
	for _, want := range []string{"Type=exec", "ExecStart=" + f.in.bin() + "\n", "Restart=always", "RestartSec=2", "User=root", "WantedBy=multi-user.target"} {
		if !strings.Contains(string(u), want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if want := []string{"daemon-reload", "enable"}; !slices.Equal(f.verbs(), want) || !slices.Contains(f.calls[1], "--now") {
		t.Fatalf("systemctl calls = %v", f.calls)
	}
	if left, _ := filepath.Glob(filepath.Join(f.in.binDir, ".glov-guest.*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestReinstallOfRunningServiceRestartsIt(t *testing.T) {
	f := newFixture(t, true)
	if err := f.in.install(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"daemon-reload", "enable", "restart"}; !slices.Equal(f.verbs(), want) {
		t.Fatalf("systemctl calls = %v", f.calls)
	}
}

func TestInstallFromTheInstalledPathDoesNotCopy(t *testing.T) {
	f := newFixture(t, false)
	os.WriteFile(f.in.bin(), []byte("in-place"), 0o755)
	f.in.self = f.in.bin()
	if err := f.in.install(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f.in.bin()); string(b) != "in-place" {
		t.Fatalf("binary rewritten: %q", b)
	}
}

func TestInstallRefusesWritableInstallDir(t *testing.T) {
	f := newFixture(t, false)
	f.in.checkOwner = true // the temp dir belongs to the test user, not root
	if os.Geteuid() == 0 {
		os.Chmod(f.in.binDir, 0o777)
	}
	if err := f.in.install(); err == nil {
		t.Fatal("installed into a directory root does not own exclusively")
	}
	if len(f.calls) != 0 {
		t.Fatalf("systemctl ran: %v", f.calls)
	}
}

func TestUninstallRemovesEverything(t *testing.T) {
	f := newFixture(t, false)
	if err := f.in.install(); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := f.in.uninstall(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.in.bin(), f.in.unit()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists", p)
		}
	}
	if want := []string{"disable", "daemon-reload"}; !slices.Equal(f.verbs(), want) {
		t.Fatalf("systemctl calls = %v", f.calls)
	}
	if err := f.in.uninstall(); err == nil {
		t.Fatal("second uninstall succeeded")
	}
}
