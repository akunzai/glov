package mirror

import "os"

// keepMode does nothing on Windows: NTFS has no permission bits to keep.
func keepMode(*os.Root, string, string) {}
