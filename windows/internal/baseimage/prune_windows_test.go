package baseimage

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPruneKeepsWhatVMsUseAndTheNewestOfEachDistro(t *testing.T) {
	c := &Cache{Dir: t.TempDir()}
	at := time.Now().Add(-time.Hour)
	release := func(name string, age time.Duration, complete bool) {
		dir := filepath.Join(c.Dir, name)
		os.MkdirAll(dir, 0o700)
		if !complete {
			os.WriteFile(filepath.Join(dir, "base.part.vhdx"), []byte("part"), 0o600)
			return
		}
		disk := filepath.Join(dir, "base.vhdx")
		os.WriteFile(disk, make([]byte, 100), 0o600)
		os.Chtimes(disk, at.Add(age), at.Add(age))
	}
	release("debian-13-genericcloud-amd64-aaaaaaaaaaaa", 0, true)              // old, used
	release("debian-13-genericcloud-amd64-bbbbbbbbbbbb", time.Minute, true)    // old, unused
	release("debian-13-genericcloud-amd64-cccccccccccc", 2*time.Minute, true)  // newest, unused
	release("debian-13-genericcloud-amd64-dddddddddddd", 3*time.Minute, false) // failed Prepare
	release("ubuntu-26.04-server-cloudimg-amd64-eeeeeeeeeeee", 0, true)        // the only ubuntu
	os.MkdirAll(filepath.Join(c.Dir, "alpine-3.24.3-x86_64-tiny-r0"), 0o700)   // no longer a distro: left alone
	os.MkdirAll(filepath.Join(c.Dir, "mine"), 0o700)                           // not a release: left alone
	os.MkdirAll(filepath.Join(c.Dir, "downloads"), 0o700)
	os.WriteFile(filepath.Join(c.Dir, "downloads", "x.qcow2.part"), []byte("x"), 0o600)

	used := func(r string) bool { return r == "debian-13-genericcloud-amd64-aaaaaaaaaaaa" }
	pruned, busy, err := c.Prune(used)
	if err != nil || busy {
		t.Fatalf("Prune: busy %v, %v", busy, err)
	}
	var names []string
	for _, p := range pruned {
		names = append(names, p.Release)
	}
	slices.Sort(names)
	want := []string{"debian-13-genericcloud-amd64-bbbbbbbbbbbb", "debian-13-genericcloud-amd64-dddddddddddd"}
	if !slices.Equal(names, want) {
		t.Errorf("pruned %v, want %v", names, want)
	}
	if pruned[0].Bytes == 0 {
		t.Errorf("pruned %+v without its size", pruned[0])
	}
	left, _ := os.ReadDir(c.Dir)
	var kept []string
	for _, e := range left {
		kept = append(kept, e.Name())
	}
	if want := []string{"alpine-3.24.3-x86_64-tiny-r0", "debian-13-genericcloud-amd64-aaaaaaaaaaaa", "debian-13-genericcloud-amd64-cccccccccccc", "mine", "ubuntu-26.04-server-cloudimg-amd64-eeeeeeeeeeee"}; !slices.Equal(kept, want) {
		t.Errorf("left %v, want %v", kept, want)
	}

	// A Prepare under way holds the cache: Prune leaves it alone.
	c.mu.Lock()
	if pruned, busy, err := c.Prune(func(string) bool { return false }); !busy || pruned != nil || err != nil {
		t.Errorf("Prune during a Prepare = %v, busy %v, %v", pruned, busy, err)
	}
	c.mu.Unlock()
}
