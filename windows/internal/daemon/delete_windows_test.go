package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/store"
)

// A live share's agent keeps its log open in the VM's directory until it
// sees the VM gone; Windows refuses to delete an open file, and delete
// failed with the VM already half gone.
func TestDeleteWaitsForAFileHeldOpenInTheVMDirectory(t *testing.T) {
	f, call := lifecycle(t)
	dir := filepath.Join(f.st.Dir("dev"), "sync")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "proj.live.log")
	if err := os.WriteFile(log, []byte("live share started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(time.Second, func() { held.Close() })
	if err := call(api.MethodVMDelete, api.VMParams{VM: "dev"}, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(f.st.Dir("dev")); !os.IsNotExist(err) {
		t.Errorf("VM directory left: %v", err)
	}
}

// While delete retries the old folder, its vm.json is gone already: a
// create of the same name must wait, or the retry removes the new VM.
func TestCreateWaitsForADeleteOfTheSameName(t *testing.T) {
	me := testOwner
	cr := &creator{st: store.Store{Root: t.TempDir()}}
	path, _ := serve(t, cr.daemon(me))
	c, err := dialNotify(t, path, me, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "dev"}, nil); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(cr.st.Dir("dev"), "sync", "proj.live.log")
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(time.Second, func() { held.Close() })
	deleted := make(chan error, 1)
	go func() { deleted <- c.Call(t.Context(), api.MethodVMDelete, api.VMParams{VM: "dev"}, nil) }()
	// Once the first removal has taken vm.json, before the log is closed.
	for deadline := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(cr.st.Dir("dev"), "vm.json")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delete did not start removing the folder")
		}
	}
	// Another connection: requests on one connection run one at a time.
	c2, err := dialNotify(t, path, me, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.Call(t.Context(), api.MethodVMCreate, api.CreateParams{VM: "dev"}, nil); err != nil {
		t.Fatalf("create again: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := cr.st.Load(me, "dev"); err != nil {
		t.Fatalf("the new VM is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cr.st.Dir("dev"), DiskFile)); err != nil {
		t.Fatalf("the new VM's disk is gone: %v", err)
	}
}
