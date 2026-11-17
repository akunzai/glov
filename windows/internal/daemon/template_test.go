package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/egress"
	"github.com/akunzai/glov/windows/internal/store"
)

func TestFreezeAndReset(t *testing.T) {
	var made, merged []string
	f, call := lifecycle(t, func(d *Daemon) {
		d.NewDisk = func(disk, parent string, size uint64) error {
			made = append(made, filepath.Base(disk)+"<"+filepath.Base(parent))
			return os.WriteFile(disk, nil, 0o600)
		}
		d.MergeDisk = func(child string) error { merged = append(merged, filepath.Base(child)); return nil }
	})
	me := testOwner
	dir := f.st.Dir("dev")
	disk, template := filepath.Join(dir, DiskFile), filepath.Join(dir, TemplateFile)
	if err := os.WriteFile(disk, []byte("installed"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := f.st.Load(me, "dev")
	// A VM made before every Windows VM was isolated.
	r.Isolated, r.Egress = false, nil
	if err := f.st.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := call(api.MethodVMFreeze, api.VMParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "not isolated") {
		t.Errorf("freeze a VM that is not isolated err = %v", err)
	}
	r.Isolated = true
	r.Egress = &store.Egress{Allow: []string{"api.anthropic.com"}, Secrets: []string{"anthropic", "gh"}}
	if err := f.st.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := call(api.MethodVMReset, api.VMParams{VM: "dev"}, nil); !isInvalid(err) || !strings.Contains(err.Error(), "has no template") {
		t.Errorf("reset before freeze err = %v", err)
	}

	var vm api.VMInfo
	if err := call(api.MethodVMFreeze, api.VMParams{VM: "dev"}, &vm); err != nil || !vm.Template {
		t.Fatalf("freeze = %+v, %v", vm, err)
	}
	if b, _ := os.ReadFile(template); string(b) != "installed" {
		t.Errorf("template holds %q, want the installed disk", b)
	}
	if !slices.Equal(made, []string{"disk.vhdx<template.vhdx"}) || len(merged) != 0 {
		t.Errorf("first freeze: made %v, merged %v", made, merged)
	}
	if err := call(api.MethodVMStart, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if s := f.started[0]; s.Template != template || s.Disk != disk {
		t.Errorf("started %+v, want the template between base and disk", s)
	}
	for _, m := range []string{api.MethodVMReset, api.MethodVMFreeze} {
		if err := call(m, api.VMParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "glov vm stop dev first") {
			t.Errorf("%s while running err = %v", m, err)
		}
	}
	if err := call(api.MethodVMStop, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}

	// The task widens the allowlist and leaves a file on the disk.
	r, _ = f.st.Load(me, "dev")
	r.Egress.Allow = append(r.Egress.Allow, "github.com")
	r.Egress.MCPEdits = true
	// It revokes one secret and is granted another: neither comes back.
	r.Egress.Secrets = []string{"anthropic", "extra"}
	if err := f.st.Save(r); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(disk, []byte("task"), 0o600)
	made = nil
	if err := call(api.MethodVMReset, api.VMParams{VM: "dev"}, &vm); err != nil || !vm.Template {
		t.Fatalf("reset = %+v, %v", vm, err)
	}
	if b, _ := os.ReadFile(disk); len(b) != 0 || !slices.Equal(made, []string{"disk.vhdx<template.vhdx"}) {
		t.Errorf("after reset the disk holds %q (made %v), want a fresh child", b, made)
	}
	r, _ = f.st.Load(me, "dev")
	if !slices.Equal(r.Egress.Allow, []string{"api.anthropic.com"}) || r.Egress.MCPEdits || !slices.Equal(r.Egress.Secrets, []string{"anthropic"}) {
		t.Errorf("egress after reset = %+v, want the frozen settings", *r.Egress)
	}
	if r.Disk != 1 {
		t.Errorf("disk after a reset = %d, want 1: live shares would keep their base", r.Disk)
	}

	// Frozen open: a VM restricted since stays restricted, a plain secret
	// allowed no more stays withheld, and a certificate removed (or
	// replaced) since stays out, as does a repository.
	r.Template.Egress.Open, r.Template.Egress.PlainSecrets = true, []string{"anthropic"}
	r.Template.Egress.Repos, r.Egress.Repos = []string{"github.com/o/kept", "github.com/o/gone"}, []string{"github.com/o/kept", "github.com/o/new"}
	r.Template.Egress.Trust = []egress.Trust{{Name: "old", Certificate: "A"}, {Name: "kept", Certificate: "B"}, {Name: "swapped", Certificate: "C"}}
	r.Egress.Open, r.Egress.PlainSecrets = false, nil
	r.Egress.Trust = []egress.Trust{{Name: "kept", Certificate: "B"}, {Name: "swapped", Certificate: "D"}}
	f.st.Save(r)
	if err := call(api.MethodVMReset, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if r, _ = f.st.Load(me, "dev"); r.Egress.Open || r.Egress.PlainSecrets != nil || !slices.Equal(r.Egress.Trust, []egress.Trust{{Name: "kept", Certificate: "B"}}) ||
		!slices.Equal(r.Egress.Repos, []string{"github.com/o/kept"}) {
		t.Errorf("restricted since the freeze, reset gives %+v", *r.Egress)
	}
	r.Egress.Open = true
	f.st.Save(r)
	if err := call(api.MethodVMReset, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if r, _ = f.st.Load(me, "dev"); !r.Egress.Open || r.Egress.PlainSecrets != nil {
		t.Errorf("open now, reset gives %+v", *r.Egress)
	}

	// Freezing again (harness install on a frozen VM) merges the child,
	// after growing the template to the child's size.
	r.DiskGiB = 12
	f.st.Save(r)
	made = nil
	if err := call(api.MethodVMFreeze, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(merged, []string{DiskFile}) || !slices.Equal(made, []string{"disk.vhdx<template.vhdx"}) || f.resized[template] != 12<<30 {
		t.Errorf("second freeze: merged %v, made %v, resized %v", merged, made, f.resized)
	}
	if err := call(api.MethodVMReset, api.VMParams{VM: "raw"}, nil); err == nil {
		t.Error("reset of a VM that is not isolated succeeded")
	}
}

func TestFreezeRefusesAStrayTemplate(t *testing.T) {
	f, call := lifecycle(t, func(d *Daemon) {
		d.NewDisk = func(disk, _ string, _ uint64) error { return os.WriteFile(disk, nil, 0o600) }
	})
	me := testOwner
	r, _ := f.st.Load(me, "dev")
	r.Isolated, r.Egress = true, &store.Egress{Allow: []string{}}
	f.st.Save(r)
	dir := f.st.Dir("dev")
	os.WriteFile(filepath.Join(dir, DiskFile), []byte("disk"), 0o600)
	os.WriteFile(filepath.Join(dir, TemplateFile), []byte("stray"), 0o600)
	if err := call(api.MethodVMFreeze, api.VMParams{VM: "dev"}, nil); err == nil || !strings.Contains(err.Error(), "no template in vm.json") {
		t.Errorf("freeze over a stray template err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, DiskFile)); string(b) != "disk" {
		t.Errorf("disk now holds %q", b)
	}
}

func TestAProjectsVMIsNeverFrozen(t *testing.T) {
	f, call := lifecycle(t, func(d *Daemon) {
		d.NewDisk = func(disk, _ string, _ uint64) error { return os.WriteFile(disk, nil, 0o600) }
	})
	r, _ := f.st.Load(testOwner, "dev")
	r.Isolated, r.Egress = true, &store.Egress{Allow: []string{}}
	r.Project = &api.Project{Path: `C:\src\p`, Sync: api.SyncGit}
	f.st.Save(r)
	os.WriteFile(filepath.Join(f.st.Dir("dev"), DiskFile), []byte("work"), 0o600)
	if err := call(api.MethodVMFreeze, api.VMParams{VM: "dev"}, nil); !isInvalid(err) || !strings.Contains(err.Error(), "never frozen") {
		t.Errorf("freeze of a project's VM err = %v", err)
	}
	var vm api.VMInfo
	if err := call(api.MethodVMStatus, api.VMParams{VM: "dev"}, &vm); err != nil || vm.Project == nil || vm.Project.Path != `C:\src\p` {
		t.Errorf("status = %+v, %v", vm.Project, err)
	}
}

func isInvalid(err error) bool {
	e, ok := errors.AsType[*proto.Error](err)
	return ok && e.Code == proto.CodeInvalidParams
}
