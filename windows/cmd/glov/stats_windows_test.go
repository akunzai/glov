package main

import (
	"strings"
	"testing"

	"github.com/akunzai/glov/skills"
	"github.com/akunzai/glov/windows/internal/api"
)

func TestRenderStats(t *testing.T) {
	var b strings.Builder
	renderStats(&b, api.Stats{Host: api.HostMemory{Physical: 32 << 30, Available: 16 << 30}})
	if !strings.Contains(b.String(), "16.0G of 32.0G available") || !strings.HasSuffix(b.String(), "no running VMs\n") {
		t.Fatalf("empty = %q", b.String())
	}
	b.Reset()
	cpu, res, alloc := 150.0, uint64(1<<29), uint64(3<<30)
	problem := "the Guest Service is too old\x1b[2J: glov vm guest update web"
	renderStats(&b, api.Stats{VMs: []api.VMStats{
		{Name: "dev", CPUs: 2, Memory: 4 << 30, CPUPercent: &cpu, MemoryResident: &res, MemoryCommitted: &res, DiskAllocated: &alloc, DiskSize: 32 << 30,
			Guest: &api.GuestStats{MemoryTotal: 4 << 30, MemoryAvailable: 3 << 30, DiskTotal: 32 << 30, DiskUsed: 2 << 30, Load: []float64{0.5, 0.25, 0}}},
		{Name: "web", CPUs: 4, Memory: 8 << 30, DiskSize: 128 << 30, GuestProblem: &problem},
	}})
	out := b.String()
	for _, want := range []string{"150% of 2", "0.5G", "3.0G/32.0G", "1.0G/3.0G", "2.0G/32.0G", "0.50 0.25 0.00",
		"web", "guest: unavailable (the Guest Service is too old[2J: glov vm guest update web)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Error("a control character reached the terminal")
	}
}

func TestGuide(t *testing.T) {
	if !strings.Contains(guide(""), "name: glov") || guide("troubleshooting") != skills.Topics["troubleshooting"] || !strings.Contains(guide("troubleshooting"), "glov vm stats") {
		t.Error("guide does not print the embedded skill")
	}
}
