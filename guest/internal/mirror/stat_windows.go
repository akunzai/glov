package mirror

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type kind int

const (
	kindOther kind = iota // reparse points (symbolic links, junctions): never touched
	kindFile
	kindDir
)

type dirEntry struct {
	name  string
	kind  kind
	size  int64
	mtime int64 // Unix nanoseconds
	key   string
}

// Offsets in FILE_ID_BOTH_DIR_INFO.
const (
	offLastWrite  = 24
	offChange     = 32
	offEndOfFile  = 40
	offAttributes = 56
	offNameLength = 60
	offFileID     = 96
	offName       = 104
)

func key(size, lastWrite, change int64, id uint64) string {
	return fmt.Sprintf("%d:%d:%d:%d", size, lastWrite, change, id)
}

// unixNano converts a FILETIME (100 ns since 1601) to Unix nanoseconds.
func unixNano(ft int64) int64 { return (ft - 116444736000000000) * 100 }

func kindOf(attrs uint32) kind {
	switch {
	case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return kindOther
	case attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		return kindDir
	}
	return kindFile
}

// listDir lists rel below r with each entry's stat key (size, LastWriteTime,
// ChangeTime, FileId) from one directory enumeration, without opening the
// entries: FileIdBothDirectoryInfo returns all of it. The directory is opened
// through r, so a parent swapped for a junction cannot redirect the listing.
func listDir(r *os.Root, rel string) ([]dirEntry, error) {
	dir := native(rel)
	f, err := r.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := windows.Handle(f.Fd())
	buf := make([]byte, 64<<10)
	var out []dirEntry
	for {
		err := windows.GetFileInformationByHandleEx(h, windows.FileIdBothDirectoryInfo, &buf[0], uint32(len(buf)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return out, nil
		}
		if err != nil {
			return nil, &os.PathError{Op: "list", Path: dir, Err: err}
		}
		for off := 0; ; {
			e := buf[off:]
			n := binary.LittleEndian.Uint32(e[offNameLength:]) / 2
			name := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&e[offName])), n))
			if name != "." && name != ".." {
				size := int64(binary.LittleEndian.Uint64(e[offEndOfFile:]))
				lastWrite := int64(binary.LittleEndian.Uint64(e[offLastWrite:]))
				out = append(out, dirEntry{name, kindOf(binary.LittleEndian.Uint32(e[offAttributes:])), size, unixNano(lastWrite),
					key(size, lastWrite, int64(binary.LittleEndian.Uint64(e[offChange:])), binary.LittleEndian.Uint64(e[offFileID:]))})
			}
			next := binary.LittleEndian.Uint32(e)
			if next == 0 {
				break
			}
			off += int(next)
		}
	}
}

// fileBasicInfo is FILE_BASIC_INFO.
type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

// fileKey is the stat key listDir would report for path.
func fileKey(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return "", &os.PathError{Op: "stat", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", &os.PathError{Op: "stat", Path: path, Err: err}
	}
	size := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
	return key(size, basic.LastWriteTime, basic.ChangeTime, uint64(info.FileIndexHigh)<<32|uint64(info.FileIndexLow)), nil
}

// entryAt stats rel below r without following it if it is a reparse point,
// from a listing of the directory holding it.
func entryAt(r *os.Root, rel string) (dirEntry, error) {
	dir, name := splitRel(rel)
	des, err := listDir(r, dir)
	if err != nil {
		return dirEntry{}, err
	}
	for _, de := range des {
		if strings.EqualFold(de.name, name) {
			return de, nil
		}
	}
	return dirEntry{}, &os.PathError{Op: "lstat", Path: rel, Err: os.ErrNotExist}
}
