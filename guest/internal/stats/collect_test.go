//go:build linux || windows

package stats

import "testing"

func TestCollectMeasuresThisMachine(t *testing.T) {
	s, err := Collect()
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryTotal == 0 || s.MemoryAvailable == 0 || s.MemoryAvailable > s.MemoryTotal {
		t.Errorf("memory %d available of %d", s.MemoryAvailable, s.MemoryTotal)
	}
	if s.DiskPath == "" || s.DiskTotal == 0 || s.DiskUsed > s.DiskTotal || s.DiskAvailable > s.DiskTotal {
		t.Errorf("disk %q: %d used, %d available of %d", s.DiskPath, s.DiskUsed, s.DiskAvailable, s.DiskTotal)
	}
	if s.CPUBusy < 0 || s.CPUBusy > 100 {
		t.Errorf("cpuBusy %v", s.CPUBusy)
	}
	t.Logf("%+v", s)
}
