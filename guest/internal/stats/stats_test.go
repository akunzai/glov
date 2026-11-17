package stats

import (
	"math"
	"strings"
	"testing"
)

func TestMeminfoGivesTotalAndAvailableInBytes(t *testing.T) {
	const meminfo = "MemTotal:        8131552 kB\nMemFree:          301234 kB\nMemAvailable:    7012345 kB\nBuffers:           12 kB\n"
	total, available, err := parseMeminfo(strings.NewReader(meminfo))
	if err != nil {
		t.Fatal(err)
	}
	if total != 8131552*1024 || available != 7012345*1024 {
		t.Fatalf("total=%d available=%d", total, available)
	}
	if _, _, err := parseMeminfo(strings.NewReader("MemTotal: 1 kB\n")); err == nil {
		t.Fatal("a meminfo without MemAvailable must fail")
	}
}

func TestLoadavgGivesTheThreeAverages(t *testing.T) {
	load, err := parseLoadavg("3.92 1.07 0.40 5/612 98765\n")
	if err != nil {
		t.Fatal(err)
	}
	if load != [3]float64{3.92, 1.07, 0.40} {
		t.Fatalf("load = %v", load)
	}
	if _, err := parseLoadavg("garbage"); err == nil {
		t.Fatal("garbage must fail")
	}
}

func TestBusyIsTheShareOfNonIdleTimeBetweenTwoSamples(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	a, err := parseProcStat("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseProcStat("cpu  400 0 150 1000 100 0 0 0 30 0\n")
	if err != nil {
		t.Fatal(err)
	}
	// busy 400 of 650 (guest time is already part of user, iowait counts as idle).
	if got := busy(a, b); math.Abs(got-61.538) > 0.01 {
		t.Fatalf("busy = %v", got)
	}
	if got := busy(b, b); got != 0 {
		t.Fatalf("busy without elapsed time = %v, want 0", got)
	}
}
