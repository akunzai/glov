// Package stats measures the guest's own resource use for guest.stats
// (docs/guest-protocol.md): memory, the system volume, load and CPU use.
package stats

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Stats is the guest.stats result. Sizes are bytes.
type Stats struct {
	MemoryTotal     uint64 `json:"memoryTotal"`
	MemoryAvailable uint64 `json:"memoryAvailable"`
	// DiskPath is the system volume: "/" or the Windows system drive.
	DiskPath      string `json:"diskPath"`
	DiskTotal     uint64 `json:"diskTotal"`
	DiskUsed      uint64 `json:"diskUsed"`
	DiskAvailable uint64 `json:"diskAvailable"`
	// Load is the 1, 5 and 15 minute load average; Windows has none (null).
	Load *[3]float64 `json:"load"`
	// CPUBusy is the percentage of all guest CPUs busy over the sample.
	CPUBusy float64 `json:"cpuBusy"`
}

// sample is how long CPU use is measured over.
const sample = 500 * time.Millisecond

// Collect measures the guest now. It takes about half a second, the CPU
// sample, so the protocol serves it beside the request loop.
func Collect() (Stats, error) { return collect(sample) }

// cpuTimes are cumulative CPU times in any unit, all CPUs together.
type cpuTimes struct{ busy, total uint64 }

// busy is the percentage of the time between a and b the CPUs were busy.
func busy(a, b cpuTimes) float64 {
	if b.total <= a.total || b.busy < a.busy {
		return 0
	}
	return 100 * float64(b.busy-a.busy) / float64(b.total-a.total)
}

// parseMeminfo reads MemTotal and MemAvailable from /proc/meminfo.
func parseMeminfo(r io.Reader) (total, available uint64, err error) {
	var haveTotal, haveAvailable bool
	s := bufio.NewScanner(r)
	for s.Scan() {
		key, rest, ok := strings.Cut(s.Text(), ":")
		if !ok || (key != "MemTotal" && key != "MemAvailable") {
			continue
		}
		kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("meminfo %s: %w", key, err)
		}
		if key == "MemTotal" {
			total, haveTotal = kb*1024, true
		} else {
			available, haveAvailable = kb*1024, true
		}
	}
	if err := s.Err(); err != nil {
		return 0, 0, err
	}
	if !haveTotal || !haveAvailable {
		return 0, 0, errors.New("meminfo has no MemTotal or MemAvailable")
	}
	return total, available, nil
}

// parseLoadavg reads the three load averages of /proc/loadavg.
func parseLoadavg(text string) (load [3]float64, err error) {
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return load, fmt.Errorf("loadavg %q", text)
	}
	for i := range 3 {
		if load[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return load, fmt.Errorf("loadavg: %w", err)
		}
	}
	return load, nil
}

// parseProcStat reads the aggregate "cpu" line of /proc/stat: user nice
// system idle iowait irq softirq steal (guest and guest_nice are already part
// of user and nice). Idle and iowait count as idle.
func parseProcStat(text string) (cpuTimes, error) {
	line, _, _ := strings.Cut(text, "\n")
	fields := strings.Fields(line)
	if len(fields) < 9 || fields[0] != "cpu" {
		return cpuTimes{}, fmt.Errorf("/proc/stat: no cpu line")
	}
	var t cpuTimes
	for i, f := range fields[1:9] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return cpuTimes{}, fmt.Errorf("/proc/stat: %w", err)
		}
		t.total += v
		if i != 3 && i != 4 {
			t.busy += v
		}
	}
	return t, nil
}
