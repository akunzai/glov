package mirror

import "os"

// StateDir holds the guest manifests of share syncs, outside every share.
var StateDir = "/var/lib/glov/sync"

// PrepareStateDir creates StateDir for root only.
func PrepareStateDir(dir string) error { return os.MkdirAll(dir, 0o700) }
