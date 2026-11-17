package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/akunzai/glov/windows/internal/api"
)

func TestDeletePrunesTheBaseImagesNoVMUses(t *testing.T) {
	cached := []string{"rel-dev", "rel-raw"}
	var d *Daemon
	_, call := lifecycle(t, func(dd *Daemon) {
		d = dd
		dd.PruneImages = func(inUse func(string) bool) (pruned []api.PrunedImage, busy bool, err error) {
			for _, r := range cached {
				if !inUse(r) {
					pruned = append(pruned, api.PrunedImage{Release: r, Bytes: 1 << 30})
				}
			}
			return pruned, false, nil
		}
	})
	me := testOwner
	setRelease := func(owner, name, rel string) {
		r, _ := d.Store.Load(owner, name)
		r.Release, r.BaseDisk = rel, filepath.Join("C:", "cache", rel, "base.vhdx")
		if rel == "" {
			r.BaseDisk = ""
		}
		d.Store.Save(r)
	}
	setRelease(me, "dev", "rel-dev")
	setRelease(me, "raw", "rel-raw")
	setRelease(otherUser, "other", "")

	// Fail closed: a VM naming no base image, or one whose record cannot
	// be read, may need any of them.
	if got := d.pruneImages(); len(got) != 0 {
		t.Errorf("pruned %v with a VM naming no base image", got)
	}
	setRelease(otherUser, "other", "rel-raw")
	record := filepath.Join(d.Store.Dir("other"), "vm.json")
	good, _ := os.ReadFile(record)
	os.WriteFile(record, []byte("{"), 0o600)
	if got := d.pruneImages(); len(got) != 0 {
		t.Errorf("pruned %v with an unreadable record", got)
	}
	os.WriteFile(record, good, 0o600)

	// While a VM is being created, its prepared release may be in no
	// record yet: nothing is pruned.
	d.creating.Add(1)
	if got := d.pruneImages(); len(got) != 0 {
		t.Errorf("pruned %v while a create runs", got)
	}
	d.creating.Add(-1)

	var res api.VMDeleted
	if err := call(api.MethodVMDelete, api.VMParams{VM: "dev"}, &res); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.PrunedImages, []api.PrunedImage{{Release: "rel-dev", Bytes: 1 << 30}}) {
		t.Errorf("vm.delete pruned %+v, want rel-dev only", res.PrunedImages)
	}
	if got := res.PrunedImages[0].Describe(); got != "removed unused base image rel-dev (1.0 GiB)" {
		t.Errorf("Describe = %q", got)
	}
	if got := (api.PrunedImage{Release: "a", Bytes: 200 << 20}).Describe(); got != "removed unused base image a (200 MiB)" {
		t.Errorf("Describe = %q", got)
	}
}
