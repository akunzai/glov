package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// suffix marks a value file.
const suffix = ".value"

// Files keeps each value in Dir as <name>.value, encrypted with DPAPI in
// user scope: only the user glovd runs as decrypts it, as on a Mac the
// login Keychain holds it. Dir lies in that user's glov home; the
// encryption keeps a copied file or backup from giving the value away to
// another account or machine. Names are case-insensitive, as NTFS is.
type Files struct {
	Dir string
	mu  sync.Mutex
}

func (f *Files) path(name string) string { return filepath.Join(f.Dir, strings.ToLower(name)+suffix) }

// entropy binds a blob to its name, so a renamed file does not read as
// another secret.
func entropy(name string) []byte { return []byte("glov secret " + strings.ToLower(name)) }

func (f *Files) Set(name, value string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	blob, err := protect([]byte(value), entropy(name))
	if err != nil {
		return fmt.Errorf("encrypt secret %s: %w", name, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	p := f.path(name)
	if err := os.WriteFile(p+".tmp", blob, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (f *Files) Get(name string) (string, bool, error) {
	if ValidName(name) != nil {
		return "", false, nil
	}
	f.mu.Lock()
	blob, err := os.ReadFile(f.path(name))
	f.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read secret %s: %w", name, err)
	}
	value, err := unprotect(blob, entropy(name))
	if err != nil {
		return "", false, fmt.Errorf("decrypt secret %s: %w", name, err)
	}
	return string(value), true, nil
}

func (f *Files) Remove(name string) (bool, error) {
	if ValidName(name) != nil {
		return false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	err := os.Remove(f.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func blob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

// take copies out and frees what DPAPI allocated, wiping it first.
func take(out windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	b := unsafe.Slice(out.Data, out.Size)
	c := slices.Clone(b)
	clear(b)
	return c
}

func protect(data, extra []byte) ([]byte, error) {
	var out windows.DataBlob
	err := windows.CryptProtectData(blob(data), nil, blob(extra), 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	if err != nil {
		return nil, err
	}
	return take(out), nil
}

func unprotect(data, extra []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(data), nil, blob(extra), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return take(out), nil
}
