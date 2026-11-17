package stats

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func collect(interval time.Duration) (Stats, error) {
	before, err := cpu()
	if err != nil {
		return Stats{}, err
	}
	s := Stats{DiskPath: "/"}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Stats{}, err
	}
	s.MemoryTotal, s.MemoryAvailable, err = parseMeminfo(f)
	f.Close()
	if err != nil {
		return Stats{}, err
	}
	// As df counts them: used excludes the blocks reserved for root, available is what others may still write.
	var fs unix.Statfs_t
	if err := unix.Statfs(s.DiskPath, &fs); err != nil {
		return Stats{}, err
	}
	bs := uint64(fs.Bsize)
	s.DiskTotal, s.DiskUsed, s.DiskAvailable = fs.Blocks*bs, (fs.Blocks-fs.Bfree)*bs, fs.Bavail*bs
	text, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return Stats{}, err
	}
	load, err := parseLoadavg(string(text))
	if err != nil {
		return Stats{}, err
	}
	s.Load = &load
	time.Sleep(interval)
	after, err := cpu()
	if err != nil {
		return Stats{}, err
	}
	s.CPUBusy = busy(before, after)
	return s, nil
}

func cpu() (cpuTimes, error) {
	text, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	return parseProcStat(string(text))
}
