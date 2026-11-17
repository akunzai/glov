//go:build unix

package mirror

import "os"

// keepMode gives the temporary file tmp the permission bits of the file it
// replaces, target, so a sync keeps a guest script executable: the host side
// (NTFS) has no such bits. A new file keeps 0644.
func keepMode(r *os.Root, target, tmp string) {
	fi, err := r.Lstat(target)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		r.Chmod(tmp, perm)
	}
}
