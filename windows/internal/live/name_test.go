package live

import (
	"errors"
	"testing"
)

func TestOnlyNamesAWindowsFolderHoldsAsThemselvesAreTaken(t *testing.T) {
	for _, p := range []string{
		"a", "a/b.jsonl", ".git/refs/heads/main", "-x-", "a b/c d", "name.with.dots", "~", "LONGFI~1.TXT",
		"CONSOLE", "COM10", "LPT", "nul_", "x.nul", "日本語/ファイル", "a..b", ".hidden",
	} {
		if err := checkName(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	for _, p := range []string{
		// what the guest's own check refuses
		"", ".", "..", "../x", "a/../b", "a/./b", "./a", "a//b", "/a", "a/", `a\b`, `..\x`, "a:b", "C:x", "x::$DATA", "a\x00b",
		// trailing dots and spaces, which Win32 drops
		"x.", "x ", "x. .", "d./f", "e /f", ".. /x", "...", ". ",
		// devices, with or without an extension
		"NUL", "nul", "CON", "con.txt", "a/PRN", "AUX.tar.gz", "COM1", "com9.log", "LPT0", "COM¹", "CONIN$", "conout$", "NUL .txt",
		// what Windows names cannot hold
		"q?", "q*", "q<", "q>", "q|", `q"`, "q\x01", "q\t", "q\x1f",
		"\xff",
	} {
		if err := checkName(p); !errors.Is(err, ErrName) {
			t.Errorf("%q: %v, want ErrName", p, err)
		}
	}
}
