//go:build unix

package egress

import (
	"os"
	"syscall"
)

// Enabled reports whether first boot made this an isolated VM: the marker and
// its ancestors are root-owned and not writable by others.
func Enabled() bool { return enabledAt([]string{"/", "/etc", "/etc/glov"}, Marker) }

func enabledAt(ancestors []string, marker string) bool {
	for _, path := range append(ancestors, marker) {
		st, err := os.Lstat(path)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
			return false
		}
		stat, ok := st.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return false
		}
	}
	b, err := os.ReadFile(marker)
	return err == nil && string(b) == Identity
}
