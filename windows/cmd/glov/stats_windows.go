package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/akunzai/glov/skills"
	"github.com/akunzai/glov/windows/internal/api"
)

// guide is the skill text glov guide prints for topic ("" for the skill).
func guide(topic string) string {
	if topic == "" {
		return skills.Skill
	}
	return skills.Topics[topic]
}

// runStats prints vm.stats once, or every 2 s (plus the 1 s sample) with
// watch until Ctrl+C.
func runStats(ctx context.Context, c caller, vm string, watch, asJSON bool) error {
	for {
		var s api.Stats
		if err := call(ctx, c, api.MethodVMStats, api.StatsParams{VM: vm}, &s); err != nil {
			if watch && ctx.Err() != nil {
				return nil
			}
			return err
		}
		if asJSON {
			opts := []json.Options{json.Deterministic(true)}
			if !watch {
				opts = append(opts, jsontext.WithIndent("  "))
			}
			out, err := json.Marshal(s, opts...)
			if err != nil {
				return err
			}
			fmt.Println(string(out))
		} else {
			if watch {
				fmt.Print("\x1b[H\x1b[2J")
			}
			renderStats(os.Stdout, s)
		}
		if !watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// renderStats writes the table macOS glov vm stats prints, with the host's
// columns of the Windows edition.
func renderStats(w io.Writer, s api.Stats) {
	h := s.Host
	fmt.Fprintf(w, "Host: memory %s of %s available, commit %s of %s\n", size(h.Available), size(h.Physical), size(h.CommitUsed), size(h.CommitLimit))
	if len(s.VMs) == 0 {
		fmt.Fprintln(w, "no running VMs")
		return
	}
	header := []string{"VM", "CPU", "MEMORY", "RESIDENT", "COMMITTED", "DISK ALLOC/SIZE", "GUEST MEM USED/AVAIL", "GUEST DISK USED/SIZE", "LOAD"}
	rows := [][]string{header}
	notes := map[int][]string{}
	for _, v := range s.VMs {
		cpu := "-"
		if v.CPUPercent != nil {
			cpu = fmt.Sprintf("%.0f%%", *v.CPUPercent)
		}
		disk := "-"
		if v.DiskAllocated != nil {
			disk = size(*v.DiskAllocated) + "/" + size(v.DiskSize)
		}
		row := []string{v.Name, cpu + " of " + fmt.Sprint(v.CPUs), size(v.Memory), sizeOr(v.MemoryResident), sizeOr(v.MemoryCommitted), disk}
		if g := v.Guest; g != nil {
			load := fmt.Sprintf("cpu %.0f%%", g.CPUBusy)
			if len(g.Load) > 0 {
				parts := make([]string, len(g.Load))
				for i, l := range g.Load {
					parts[i] = fmt.Sprintf("%.2f", l)
				}
				load = strings.Join(parts, " ")
			}
			row = append(row, size(g.MemoryTotal-min(g.MemoryAvailable, g.MemoryTotal))+"/"+size(g.MemoryAvailable), size(g.DiskUsed)+"/"+size(g.DiskTotal), load)
		} else {
			row = append(row, "-", "-", "-")
		}
		var n []string
		if v.HostProblem != nil {
			n = append(n, "host: "+printable(*v.HostProblem))
		}
		if v.GuestProblem != nil {
			n = append(n, "guest: unavailable ("+printable(*v.GuestProblem)+")")
		}
		notes[len(rows)] = n
		rows = append(rows, row)
	}
	widths := make([]int, len(header))
	for _, r := range rows {
		for i, cell := range r {
			widths[i] = max(widths[i], len(cell))
		}
	}
	for i, r := range rows {
		for j, cell := range r {
			if j == len(r)-1 {
				fmt.Fprintln(w, cell)
			} else {
				fmt.Fprintf(w, "%-*s  ", widths[j], cell)
			}
		}
		for _, n := range notes[i] {
			fmt.Fprintln(w, "  "+n)
		}
	}
}

func size(b uint64) string { return fmt.Sprintf("%.1fG", float64(b)/(1<<30)) }

func sizeOr(b *uint64) string {
	if b == nil {
		return "-"
	}
	return size(*b)
}

// printable drops control characters: a problem can quote the guest's own
// error text, which must not drive the terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
