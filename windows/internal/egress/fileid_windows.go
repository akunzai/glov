package egress

import (
	"os"

	"golang.org/x/sys/windows"
)

// fileID is f's NTFS file index, which a rename keeps; creation times do
// not tell files apart, since file tunneling hands a new file the creation
// time of the one it replaced.
func fileID(f *os.File) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}
