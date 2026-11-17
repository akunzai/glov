//go:build !windows && !linux

package mirror

import (
	"os"
	"path/filepath"
)

// StateDir holds the guest manifests of share syncs; off Linux and Windows,
// where the Guest Service only runs in tests, it is a temporary directory.
var StateDir = filepath.Join(os.TempDir(), "glov-sync")

// PrepareStateDir creates StateDir for its owner only.
func PrepareStateDir(dir string) error { return os.MkdirAll(dir, 0o700) }
