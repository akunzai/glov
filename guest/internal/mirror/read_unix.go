//go:build unix

package mirror

import (
	"errors"
	"io"

	"github.com/akunzai/glov/guest/proto"
	"golang.org/x/sys/unix"
)

// readChunk bounds one sync.read answer's data, so it fits the 1 MiB frame
// as base64.
const readChunk = 512 << 10

// ReadParams are sync.read's params.
type ReadParams struct {
	ID     string `json:"id"`
	Root   string `json:"root"`
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

// ReadResult is one chunk of a file below a share's root. Key is the file's
// stat key as the chunk was read: the host reads the file again when it
// changes between chunks.
type ReadResult struct {
	Data []byte `json:"data"`
	Size int64  `json:"size"`
	Key  string `json:"key"`
	EOF  bool   `json:"eof,omitzero"`
}

// Read returns the chunk of a file at offset, reached as a listing would
// reach it: through the share's root, never through a link.
func (m *Manager) Read(p ReadParams) (ReadResult, error) {
	if err := errors.Join(checkID(p.ID), checkRoot(p.Root), checkRel(p.Path)); err != nil {
		return ReadResult{}, err
	}
	if p.Path == "" || p.Offset < 0 {
		return ReadResult{}, proto.Errorf(proto.CodeInvalidParams, "sync.read needs a file below the root and an offset of 0 or more")
	}
	dir, err := openShareRoot(p.Root, false)
	if err != nil {
		return ReadResult{}, proto.FromOS(err)
	}
	defer dir.Close()
	de, ok, err := reach(dir, p.Path)
	if err != nil {
		return ReadResult{}, proto.FromOS(err)
	}
	if !ok || de.kind != kindFile {
		return ReadResult{}, proto.Errorf(proto.CodeOS, "%s is no file below the share's root", p.Path)
	}
	// The name may have been swapped for a link since reach: the os.Root
	// keeps the open inside the root, and the file opened must be a regular
	// file; O_NONBLOCK keeps a FIFO swapped in from blocking the open.
	f, err := dir.OpenFile(native(p.Path), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return ReadResult{}, proto.FromOS(err)
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return ReadResult{}, proto.FromOS(err)
	}
	if kindOf(uint32(st.Mode)) != kindFile {
		return ReadResult{}, proto.Errorf(proto.CodeOS, "%s is no file below the share's root", p.Path)
	}
	buf := make([]byte, readChunk)
	n, err := f.ReadAt(buf, p.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return ReadResult{}, proto.FromOS(err)
	}
	return ReadResult{Data: buf[:n], Size: st.Size, Key: keyOf(&st), EOF: p.Offset+int64(n) >= st.Size}, nil
}
