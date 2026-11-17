package main

import (
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/akunzai/glov/windows/internal/api"
	"github.com/akunzai/glov/windows/internal/secret"
)

// sizeText is a byte count as glov prints it.
func sizeText(bytes int) string {
	if bytes < 1<<20 {
		return fmt.Sprintf("%d KiB", (bytes+1023)>>10)
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(1<<20))
}

// recordsUsage counts the regular files below dir and their bytes. Links
// and junctions are neither followed nor counted: the guest writes into this
// folder and could point one anywhere. ok is false when dir does not exist.
func recordsUsage(dir string) (bytes, files int, ok bool) {
	if _, err := os.Lstat(dir); err != nil {
		return 0, 0, false
	}
	_ = fs.WalkDir(os.DirFS(dir), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files++
			bytes += int(info.Size())
		}
		return nil
	})
	return bytes, files, true
}

// describeRecords is the conversation records line of glov vm status.
func describeRecords(dir string, bytes, files int, exists bool) string {
	used := "none yet"
	if exists {
		used = fmt.Sprintf("%d files, %s", files, sizeText(bytes))
	}
	return fmt.Sprintf("  conversation records: %s (%s)", dir, used)
}

// keptRecordsLine is what glov vm delete says about the records folder it
// leaves behind.
func keptRecordsLine(dir string) string {
	return fmt.Sprintf("kept its conversation records at %s; remove them yourself when no longer needed", dir)
}

// sourcesToRefresh selects, from the stored secrets, the command-sourced
// ones among granted that are not in done, each once.
func sourcesToRefresh(list []api.Secret, granted []string, done map[string]bool) []api.Secret {
	return slices.DeleteFunc(commandSourcesIn(list, granted), func(s api.Secret) bool { return done[s.Name] })
}

// commandSourcesIn selects, from the stored secrets, those taken from a
// command among names: the ones to run again before they are granted.
func commandSourcesIn(list []api.Secret, names []string) []api.Secret {
	return slices.DeleteFunc(slices.Clone(list), func(s api.Secret) bool {
		return s.Source != secret.Command || !slices.Contains(names, s.Name)
	})
}
