package mirror

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akunzai/glov/guest/proto"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

func checkID(id string) error {
	if !idPattern.MatchString(id) {
		return proto.Errorf(proto.CodeInvalidParams, "invalid sync id %q", id)
	}
	return nil
}

func checkRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return proto.Errorf(proto.CodeInvalidParams, "root must be an absolute path, got %q", root)
	}
	return nil
}

// checkRel accepts a path relative to the root: '/'-separated, no empty, "."
// or ".." segments, no ':' (alternate data streams, drive letters) and no
// backslash. The empty path is the root itself.
func checkRel(rel string) error {
	if rel == "" {
		return nil
	}
	if strings.ContainsAny(rel, ":\\\x00") {
		return proto.Errorf(proto.CodeInvalidParams, "invalid path %q", rel)
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return proto.Errorf(proto.CodeInvalidParams, "invalid path %q", rel)
		}
	}
	return nil
}

func join(root, rel string) string {
	if rel == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// inScope reports whether rel is scope itself or lies below it.
func inScope(rel, scope string) bool {
	return scope == "" || rel == scope || strings.HasPrefix(rel, scope+"/")
}

// splitRel splits rel into its directory ("" for the root) and last name.
func splitRel(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, '/')
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

// reach stats rel below r when every directory above it is a real directory
// (no link): ok is false when rel, or a directory on the way, is missing or
// something else, which a listing would not reach either.
func reach(r *os.Root, rel string) (e dirEntry, ok bool, err error) {
	at := ""
	for name := range strings.SplitSeq(rel, "/") {
		at = joinRel(at, name)
		e, err = entryAt(r, at)
		if errors.Is(err, fs.ErrNotExist) {
			return dirEntry{}, false, nil
		}
		if err != nil {
			return dirEntry{}, false, err
		}
		if at != rel && e.kind != kindDir {
			return dirEntry{}, false, nil
		}
	}
	return e, e.kind != kindOther, nil
}
