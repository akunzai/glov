//go:build !linux

package mirror

// watcher exists on Linux only: sync.watch is served there alone.
type watcher struct{}
