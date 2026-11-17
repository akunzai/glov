package mirror

import (
	"path/filepath"

	"github.com/akunzai/glov/guest/internal/logfile"
)

// StateDir holds the guest manifests of share syncs, outside every share.
var StateDir = `C:\ProgramData\glov\sync`

// PrepareStateDir creates C:\ProgramData\glov and its sync directory for
// SYSTEM and Administrators only, refusing a pre-planted or linked one.
func PrepareStateDir(dir string) error {
	if err := logfile.PrepareDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return logfile.PrepareDir(dir)
}
