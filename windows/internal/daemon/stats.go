package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/akunzai/glov/guest/proto"
	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/store"
)

// statsSample is how long vm.stats measures CPU use over, as on macOS.
const statsSample = time.Second

// statsGuestTimeout bounds guest.stats, which samples for half a second.
const statsGuestTimeout = 5 * time.Second

// stats measures the host and each running VM (only p.VM, if given).
func (d *Daemon) stats(ctx context.Context, caller string, p api.StatsParams) (api.Stats, error) {
	var recs []store.Record
	if p.VM != "" {
		r, err := d.load(caller, p.VM)
		if err != nil {
			return api.Stats{}, err
		}
		if !d.VMs.Running(r.ID) {
			return api.Stats{}, fmt.Errorf("VM %s is not running: glov vm start %s", r.Name, r.Name)
		}
		recs = append(recs, r)
	} else {
		names, err := d.Store.Names(caller)
		if err != nil {
			return api.Stats{}, err
		}
		for _, n := range names {
			if r, err := d.Store.Load(caller, n); err == nil && d.VMs.Running(r.ID) {
				recs = append(recs, r)
			}
		}
	}
	host, err := d.HostMemory()
	if err != nil {
		return api.Stats{}, fmt.Errorf("host memory: %w", err)
	}
	vms := make([]api.VMStats, len(recs))
	var wg sync.WaitGroup
	for i, r := range recs {
		wg.Go(func() { vms[i] = d.vmStats(ctx, r) })
	}
	wg.Wait()
	return api.Stats{Host: host, VMs: vms}, nil
}

// vmStats measures one running VM: its CPU use over statsSample while the
// guest measures itself.
func (d *Daemon) vmStats(ctx context.Context, r store.Record) api.VMStats {
	s := api.VMStats{Name: r.Name, CPUs: r.CPUs, Memory: uint64(r.MemoryMiB) << 20, DiskSize: uint64(r.DiskGiB) << 30}
	var problems []string
	if fi, err := os.Stat(filepath.Join(d.Store.Dir(r.Name), DiskFile)); err == nil {
		n := uint64(fi.Size())
		s.DiskAllocated = &n
	} else {
		problems = append(problems, "disk: "+err.Error())
	}
	type guestResult struct {
		stats   *api.GuestStats
		problem string
	}
	guest := make(chan guestResult, 1)
	go func() {
		gctx, cancel := context.WithTimeout(ctx, statsGuestTimeout)
		defer cancel()
		var g api.GuestStats
		if err := d.VMs.GuestCall(gctx, r.ID, "guest.stats", nil, &g); err != nil {
			guest <- guestResult{problem: guestStatsProblem(r, err)}
			return
		}
		guest <- guestResult{stats: &g}
	}()
	start := time.Now()
	u0, err0 := d.VMs.Usage(r.ID)
	select {
	case <-time.After(statsSample):
	case <-ctx.Done():
	}
	u1, err1 := d.VMs.Usage(r.ID)
	elapsed := time.Since(start)
	switch {
	case err0 != nil || err1 != nil:
		problems = append(problems, errors.Join(err0, err1).Error())
	default:
		pct := float64(u1.CPUTime-u0.CPUTime) / float64(elapsed) * 100
		s.CPUPercent = &pct
		s.MemoryResident, s.MemoryCommitted = u1.Resident, u1.Committed
	}
	if len(problems) > 0 {
		p := strings.Join(problems, "; ")
		s.HostProblem = &p
	}
	g := <-guest
	s.Guest = g.stats
	if g.problem != "" {
		s.GuestProblem = &g.problem
	}
	return s
}

func guestStatsProblem(r store.Record, err error) string {
	if e, ok := errors.AsType[*proto.Error](err); ok && e.Code == proto.CodeNoMethod {
		return "the Guest Service is too old: glov vm guest update " + r.Name
	}
	return err.Error()
}
