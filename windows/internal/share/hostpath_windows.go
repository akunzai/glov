package share

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Canonical returns the folder path as Windows names it (drive letter and
// each name in the case the file system stores, a subst drive resolved), or
// why it cannot be a share: it must be an existing directory on a local
// drive, and neither it nor a folder above it may be a symbolic link or
// junction, since a sync and the copy allowlist would follow it.
func Canonical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(abs) || len(filepath.VolumeName(abs)) != 2 {
		return "", fmt.Errorf("host path %s is not on a drive, such as C:\\src\\proj", path)
	}
	// Each prefix is opened itself, not the target of a link at it.
	prefix := filepath.VolumeName(abs) + `\`
	for _, name := range strings.Split(strings.Trim(abs[len(prefix):], `\`), `\`) {
		if name == "" {
			break
		}
		prefix = filepath.Join(prefix, name)
		dir, link, err := inspect(prefix)
		if err != nil {
			return "", err
		}
		if link {
			return "", fmt.Errorf("host path %s is or lies below a symbolic link or junction (%s); share the folder it points to", path, prefix)
		}
		if !dir {
			return "", fmt.Errorf("host path %s is not a directory", path)
		}
	}
	final, err := finalPath(abs)
	if err != nil {
		return "", err
	}
	if _, err := GuestPath(final); err != nil {
		return "", err
	}
	return final, nil
}

// Resolve returns path's canonical form for the copy allowlist: links in the
// part that exists are followed, and the part that does not exist yet is
// appended (filepath.Abs has removed any "..").
func Resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Refuse UNC and device paths before touching them: a stat of
	// \\host\share would sign in to that host with the user's credentials.
	if len(filepath.VolumeName(abs)) != 2 {
		return "", fmt.Errorf("host path %s is not on a drive, such as C:\\src\\proj", path)
	}
	existing, tail := abs, []string{}
	for {
		if _, err := os.Stat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("host path %s: no part of it exists", path)
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		existing = parent
	}
	final, err := finalPath(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{final}, tail...)...), nil
}

// longPath lets the Win32 API open an absolute path beyond MAX_PATH.
func longPath(abs string) (*uint16, error) {
	return windows.UTF16PtrFromString(`\\?\` + abs)
}

// inspect reports whether path is a directory and whether it is a name
// surrogate (a symbolic link, junction or mount point), without following it.
// Other reparse points, such as OneDrive's placeholders, are plain folders.
func inspect(path string) (dir, link bool, err error) {
	p, err := longPath(path)
	if err != nil {
		return false, false, err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, false, fmt.Errorf("host path %s does not exist", path)
		}
		return false, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)
	var info struct {
		Attributes uint32
		ReparseTag uint32
	}
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return false, false, fmt.Errorf("read %s: %w", path, err)
	}
	const nameSurrogate = 0x20000000
	link = info.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && info.ReparseTag&nameSurrogate != 0
	return info.Attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0, link, nil
}

// finalPath is the path Windows reports for an open handle on abs.
func finalPath(abs string) (string, error) {
	p, err := longPath(abs)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", abs, err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0) // FILE_NAME_NORMALIZED | VOLUME_NAME_DOS
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", abs, err)
	}
	final := windows.UTF16ToString(buf[:n])
	if strings.HasPrefix(final, `\\?\UNC\`) {
		return "", fmt.Errorf("host path %s is on a network drive; share a folder on a local drive", abs)
	}
	return strings.TrimPrefix(final, `\\?\`), nil
}

// HostProtected are the folders of the user running glov that a share may
// not be or touch: the profile itself, glov's home (VM disks, secrets, SSH
// keys) and folders of credentials. glovd adds its data and image cache
// directories.
func HostProtected() []Protected {
	profile, _ := os.UserHomeDir()
	local, _ := os.UserCacheDir()
	roaming, _ := os.UserConfigDir()
	var p []Protected
	add := func(path string, tree bool, why string) {
		if path == "" || !filepath.IsAbs(path) {
			return
		}
		if final, err := Resolve(path); err == nil {
			path = final
		}
		p = append(p, Protected{Path: path, Tree: tree, Why: why})
	}
	add(profile, false, "your whole user profile")
	if local != "" {
		add(filepath.Join(local, "glov"), true, "glov's home, which holds VM disks, secrets and SSH keys")
	}
	add(os.Getenv("GLOV_HOME"), true, "glov's home, which holds VM disks and secrets")
	if profile != "" {
		for _, d := range []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", `.config\gcloud`} {
			add(filepath.Join(profile, d), true, "a folder of credentials")
		}
	}
	if roaming != "" {
		add(filepath.Join(roaming, "gnupg"), true, "a folder of credentials")
	}
	return p
}
