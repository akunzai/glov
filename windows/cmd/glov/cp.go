package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// chunk is the most file.read returns at once (docs/guest-protocol.md).
const chunk = 512 << 10

// guestFS is a VM's file system through the Guest Service's file methods.
type guestFS interface {
	call(method string, params, result any) error
	// mkdirs creates directories, with their parents.
	mkdirs(dirs []string) error
}

type fileStat struct {
	Exists bool  `json:"exists"`
	IsDir  bool  `json:"isDir"`
	Size   int64 `json:"size"`
}

type fileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

// copied counts what glov cp moved; skipped lists what it left out.
type copied struct {
	files   int
	bytes   int64
	skipped []string
}

// toGuest copies the host file or tree src to the guest path dst; a
// directory's contents land in dst.
func toGuest(g guestFS, src, dst string) (copied, error) {
	var c copied
	fi, err := os.Stat(src)
	if err != nil {
		return c, err
	}
	if !fi.IsDir() {
		return c, putFile(g, src, dst, &c)
	}
	var dirs []string
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := path.Join(dst, filepath.ToSlash(rel))
		switch {
		case d.IsDir():
			dirs = append(dirs, to)
		case d.Type().IsRegular():
			return putFile(g, p, to, &c)
		default:
			c.skipped = append(c.skipped, p)
		}
		return nil
	})
	if err != nil {
		return c, err
	}
	// file.write creates a file's parents; this also creates the empty
	// directories.
	return c, g.mkdirs(dirs)
}

func putFile(g guestFS, src, dst string, c *copied) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, chunk)
	var off int64
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 || off == 0 {
			p := map[string]any{"path": dst, "offset": off, "data": buf[:n], "mkdirs": true}
			if off == 0 {
				p["truncate"] = true
			}
			if werr := g.call("file.write", p, nil); werr != nil {
				return fmt.Errorf("%s: %w", dst, werr)
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	c.files++
	c.bytes += off
	return nil
}

// fromGuest copies the guest file or tree src to the host path dst; a
// directory's contents land in dst.
func fromGuest(g guestFS, src, dst string) (copied, error) {
	var c copied
	var st fileStat
	if err := g.call("file.stat", map[string]string{"path": src}, &st); err != nil {
		return c, err
	}
	if !st.Exists {
		return c, fmt.Errorf("%s does not exist in the guest", src)
	}
	if !st.IsDir {
		return c, getFile(g, src, dst, &c)
	}
	return c, getTree(g, src, dst, &c)
}

func getTree(g guestFS, src, dst string, c *copied) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var list struct {
		Entries []fileEntry `json:"entries"`
	}
	if err := g.call("file.list", map[string]string{"path": src}, &list); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	for _, e := range list.Entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." || filepath.Base(e.Name) != e.Name {
			c.skipped = append(c.skipped, path.Join(src, e.Name))
			continue
		}
		from, to := path.Join(src, e.Name), filepath.Join(dst, e.Name)
		// Never write through a link or junction already in the host tree:
		// it could lead out of the folder the copy allowlist let in.
		if fi, err := os.Lstat(to); err == nil && !(e.IsDir && fi.IsDir() || !e.IsDir && fi.Mode().IsRegular()) {
			c.skipped = append(c.skipped, to)
			continue
		}
		var err error
		if e.IsDir {
			err = getTree(g, from, to, c)
		} else {
			err = getFile(g, from, to, c)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func getFile(g guestFS, src, dst string, c *copied) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	var off int64
	for {
		var r struct {
			Data []byte `json:"data"`
			EOF  bool   `json:"eof"`
		}
		if err := g.call("file.read", map[string]any{"path": src, "offset": off, "length": chunk}, &r); err != nil {
			f.Close()
			return fmt.Errorf("%s: %w", src, err)
		}
		if _, err := f.Write(r.Data); err != nil {
			f.Close()
			return err
		}
		off += int64(len(r.Data))
		if r.EOF || len(r.Data) == 0 {
			break
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	c.files++
	c.bytes += off
	return nil
}
