package guestservice_test

import (
	"encoding/json/v2"
	"testing"
)

func TestStatsOverTheWire(t *testing.T) {
	h := start(t)
	h.connect()
	var s struct {
		MemoryTotal     uint64      `json:"memoryTotal"`
		MemoryAvailable uint64      `json:"memoryAvailable"`
		DiskTotal       uint64      `json:"diskTotal"`
		DiskPath        string      `json:"diskPath"`
		Load            *[3]float64 `json:"load"`
	}
	if err := json.Unmarshal(h.mustCall("guest.stats", `{}`).Result, &s); err != nil {
		t.Fatal(err)
	}
	if s.MemoryTotal == 0 || s.DiskTotal == 0 || s.DiskPath != "/" || s.Load == nil {
		t.Fatalf("guest.stats = %+v", s)
	}
}
