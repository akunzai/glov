package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const alice, bob = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-1002"

func TestOwnersSeeOnlyTheirVMs(t *testing.T) {
	s := Store{Root: t.TempDir()}
	for _, r := range []Record{{Name: "dev", Owner: alice}, {Name: "devbox", Owner: alice}, {Name: "web", Owner: bob}} {
		if err := s.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	names, err := s.Names(alice)
	if err != nil || !slices.Equal(names, []string{"dev", "devbox"}) {
		t.Fatalf("Names(alice) = %v, %v", names, err)
	}
	if _, err := s.Load(alice, "web"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load(alice, bob's VM) err = %v, want not exist", err)
	}
	if _, err := s.Resolve(alice, "w"); err == nil {
		t.Fatal("Resolve found bob's VM for alice")
	}
	if got, err := s.Resolve(alice, "devb"); err != nil || got != "devbox" {
		t.Fatalf("Resolve(devb) = %q, %v", got, err)
	}
	if r, err := s.Load(bob, "web"); err != nil || r.Owner != bob {
		t.Fatalf("Load(bob, web) = %+v, %v", r, err)
	}
}

func TestCreateRefusesTakenAndInvalidNames(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Create(Record{Name: "dev", Owner: alice}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(Record{Name: "dev", Owner: bob}); !errors.Is(err, ErrExists) {
		t.Fatalf("second create err = %v, want ErrExists", err)
	}
	for _, name := range []string{"..", `..\x`, "Dev", "con"} {
		if err := s.Create(Record{Name: name, Owner: alice}); err == nil {
			t.Errorf("Create(%q) succeeded", name)
		}
	}
	if err := s.Create(Record{Name: "x"}); err == nil {
		t.Error("Create without an owner succeeded")
	}
}

func TestNamesSkipsStrayDirectories(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Create(Record{Name: "dev", Owner: alice}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"Bad Name", "empty", "renamed"} {
		if err := os.MkdirAll(filepath.Join(s.Root, "vms", d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A vm.json copied under another directory name is not trusted.
	data, _ := os.ReadFile(filepath.Join(s.Dir("dev"), "vm.json"))
	os.WriteFile(filepath.Join(s.Dir("renamed"), "vm.json"), data, 0o600)
	names, err := s.Names(alice)
	if err != nil || !slices.Equal(names, []string{"dev"}) {
		t.Fatalf("Names = %v, %v", names, err)
	}
}

func TestRecordsListsEveryOwner(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if r, err := s.Records(); err != nil || r != nil {
		t.Fatalf("no vms dir: %v, %v", r, err)
	}
	s.Create(Record{Name: "web", Owner: bob})
	s.Create(Record{Name: "dev", Owner: alice})
	os.Mkdir(filepath.Join(s.Root, "vms", "broken"), 0o700)
	r, err := s.Records()
	if err != nil || len(r) != 2 || r[0].Name != "dev" || r[1].Name != "web" {
		t.Fatalf("Records = %+v, %v", r, err)
	}
}

func TestSaveReplacesAnExistingRecordOnly(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.Save(Record{Name: "dev", Owner: alice}); err == nil {
		t.Fatal("Save created a VM")
	}
	if err := s.Create(Record{Name: "dev", Owner: alice, ID: "id"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Record{Name: "dev", Owner: alice, ID: "id", Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Load(alice, "dev"); err != nil || !r.Provisioned || r.ID != "id" {
		t.Fatalf("Load after Save = %+v, %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir("dev"), "vm.json.tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("vm.json.tmp left behind: %v", err)
	}
}

func TestAFolderWithoutARecordIsNoVM(t *testing.T) {
	s := Store{Root: t.TempDir()}
	// glov's live share state for a VM deleted, or not yet created.
	if err := os.MkdirAll(filepath.Join(s.Dir("dev"), "sync"), 0o700); err != nil {
		t.Fatal(err)
	}
	if recs, err := s.AllRecords(); err != nil || len(recs) != 0 {
		t.Fatalf("AllRecords = %v, %v; want none", recs, err)
	}
	if err := s.Create(Record{Name: "dev", Owner: "S-1-5-21-1"}); err != nil {
		t.Fatalf("Create over a folder without a record: %v", err)
	}
	if err := s.Create(Record{Name: "dev", Owner: "S-1-5-21-2"}); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir("dev"), "sync")); err != nil {
		t.Fatalf("the live share state went: %v", err)
	}
}
