package daemon

import (
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/share"
)

// keepsConcurrentChange holds vm's lock while run starts and blocks on it,
// adds a share to vm's record as another method would, then lets run go on:
// the share must survive what run saves.
func keepsConcurrentChange(t *testing.T, d *Daemon, f *fakeRunner, vm string, run func() error) {
	t.Helper()
	unlock := d.lock(vm)
	done := make(chan error, 1)
	go func() { done <- run() }()
	// Long enough for run to read a record ahead of the lock, as before
	// the fix; with the fix it reads only after unlock.
	time.Sleep(300 * time.Millisecond)
	r, err := f.st.Load(testOwner, vm)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	r.Shares = append(r.Shares, share.Share{Name: "late", HostPath: `C:\src\Late`})
	if err := f.st.Save(r); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r, err = f.st.Load(testOwner, vm); err != nil || len(r.Shares) != 1 || r.Shares[0].Name != "late" {
		t.Errorf("shares after the change = %+v, %v", r.Shares, err)
	}
}

func TestConfigureKeepsAChangeMadeWhileItWaitedForTheLock(t *testing.T) {
	var d *Daemon
	f, call := lifecycle(t, func(dd *Daemon) { d = dd })
	keepsConcurrentChange(t, d, f, "dev", func() error {
		return call(api.MethodVMConfigure, api.ConfigureParams{VM: "dev", CPUs: 1}, nil)
	})
	if r, _ := f.st.Load(testOwner, "dev"); r.CPUs != 1 {
		t.Errorf("cpus = %d", r.CPUs)
	}
}
