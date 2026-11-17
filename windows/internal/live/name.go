package live

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// checkName refuses, with ErrName, a path from the guest that a Windows
// folder cannot hold as itself, before the host's file system sees it: the
// guest is assumed hostile, and Win32 would read such a name as another
// path (a separator, a drive, a stream, a device) or as another name
// (trailing dots and spaces are dropped). It mirrors the Guest Service's
// own check (guest/internal/mirror/paths.go checkRel) and adds Windows'
// rules. It refuses rather than changes a name; os.Root keeps whatever gets
// through inside the root regardless.
func checkName(p string) error {
	if p == "" || !utf8.ValidString(p) {
		return fmt.Errorf("%q: %w", p, ErrName)
	}
	for _, r := range p {
		if r < 0x20 || strings.ContainsRune(`\:*?"<>|`, r) {
			return fmt.Errorf("%q: %w", p, ErrName)
		}
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") || device(seg) {
			return fmt.Errorf("%q: %w", p, ErrName)
		}
	}
	return nil
}

// device reports whether Windows may take seg for a device: CON, PRN, AUX,
// NUL, COM0-9, LPT0-9 (and COM¹ and the like), CONIN$ or CONOUT$, with any
// extension, as Windows before 11 does.
func device(seg string) bool {
	base, _, _ := strings.Cut(seg, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if !strings.HasPrefix(base, "COM") && !strings.HasPrefix(base, "LPT") {
		return false
	}
	switch base[3:] {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}
