// Package files implements the file.* methods. File contents are never logged.
package files

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akunzai/glov/guest/proto"
)

// MaxRead is the largest length file.read accepts.
const MaxRead = 524288

type PathParams struct {
	Path string `json:"path"`
}

type StatResult struct {
	Exists bool       `json:"exists"`
	IsDir  bool       `json:"isDir"`
	Size   int64      `json:"size"`
	MTime  *time.Time `json:"mtime"` // null when the path does not exist
}

func Stat(p PathParams) (StatResult, error) {
	if err := needPath(p.Path); err != nil {
		return StatResult{}, err
	}
	fi, err := os.Stat(p.Path)
	if os.IsNotExist(err) {
		return StatResult{}, nil
	}
	if err != nil {
		return StatResult{}, proto.FromOS(err)
	}
	return StatResult{true, fi.IsDir(), fi.Size(), utc(fi.ModTime())}, nil
}

type Entry struct {
	Name  string    `json:"name"`
	IsDir bool      `json:"isDir"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

type ListResult struct {
	Entries []Entry `json:"entries"`
}

func List(p PathParams) (ListResult, error) {
	if err := needPath(p.Path); err != nil {
		return ListResult{}, err
	}
	des, err := os.ReadDir(p.Path)
	if err != nil {
		return ListResult{}, proto.FromOS(err)
	}
	res := ListResult{Entries: []Entry{}}
	for _, de := range des {
		fi, err := de.Info()
		if err != nil { // removed while listing
			continue
		}
		res.Entries = append(res.Entries, Entry{de.Name(), fi.IsDir(), fi.Size(), *utc(fi.ModTime())})
	}
	return res, nil
}

type ReadParams struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

type ReadResult struct {
	Data []byte `json:"data"`
	EOF  bool   `json:"eof"`
}

func Read(p ReadParams) (ReadResult, error) {
	if err := needPath(p.Path); err != nil {
		return ReadResult{}, err
	}
	if p.Offset < 0 || p.Length < 0 || p.Length > MaxRead {
		return ReadResult{}, proto.Errorf(proto.CodeInvalidParams, "offset must be >= 0 and length within 0..%d", MaxRead)
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return ReadResult{}, proto.FromOS(err)
	}
	defer f.Close()
	buf := make([]byte, p.Length)
	n, err := f.ReadAt(buf, p.Offset)
	if err != nil && err != io.EOF {
		return ReadResult{}, proto.FromOS(err)
	}
	fi, err := f.Stat()
	if err != nil {
		return ReadResult{}, proto.FromOS(err)
	}
	return ReadResult{buf[:n], p.Offset+int64(n) >= fi.Size()}, nil
}

type WriteParams struct {
	Path     string `json:"path"`
	Offset   int64  `json:"offset"`
	Data     []byte `json:"data"`
	Truncate bool   `json:"truncate"`
	Mkdirs   bool   `json:"mkdirs"`
}

type WriteResult struct {
	Size int64 `json:"size"`
}

func Write(p WriteParams) (WriteResult, error) {
	if err := needPath(p.Path); err != nil {
		return WriteResult{}, err
	}
	if p.Offset < 0 || p.Truncate && p.Offset != 0 {
		return WriteResult{}, proto.Errorf(proto.CodeInvalidParams, "truncate is only valid at offset 0")
	}
	if p.Mkdirs {
		if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
			return WriteResult{}, proto.FromOS(err)
		}
	}
	flags := os.O_WRONLY | os.O_CREATE
	if p.Truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(p.Path, flags, 0o644)
	if err != nil {
		return WriteResult{}, proto.FromOS(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(p.Data, p.Offset); err != nil {
		return WriteResult{}, proto.FromOS(err)
	}
	fi, err := f.Stat()
	if err != nil {
		return WriteResult{}, proto.FromOS(err)
	}
	return WriteResult{fi.Size()}, nil
}

type RemoveParams struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

func Remove(p RemoveParams) (struct{}, error) {
	if err := needPath(p.Path); err != nil {
		return struct{}{}, err
	}
	if _, err := os.Lstat(p.Path); err != nil { // RemoveAll would report success for a missing path
		return struct{}{}, proto.FromOS(err)
	}
	remove := os.Remove
	if p.Recursive {
		remove = os.RemoveAll
	}
	if err := remove(p.Path); err != nil {
		return struct{}{}, proto.FromOS(err)
	}
	return struct{}{}, nil
}

func needPath(path string) error {
	if path == "" {
		return proto.Errorf(proto.CodeInvalidParams, "path is required")
	}
	return nil
}

func utc(t time.Time) *time.Time { t = t.UTC(); return &t }
