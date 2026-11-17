package vmname

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for _, name := range []string{"dev", "a", "web-1", "alpine_3.24", "0x", "con1", "nul-box", strings.Repeat("a", MaxLen)} {
		if err := Valid(name); err != nil {
			t.Errorf("Valid(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		"", "Dev", ".hidden", "-x", "_x", "trailing.", "a b", `a\b`, "a/b", "a:b", "ü",
		"con", "CON", "nul.txt", "com1", "lpt9", "com¹", strings.Repeat("a", MaxLen+1),
	} {
		if err := Valid(name); err == nil {
			t.Errorf("Valid(%q) = nil, want an error", name)
		}
	}
}

func TestResolve(t *testing.T) {
	names := []string{"dev", "devbox", "web"}
	for ref, want := range map[string]string{"dev": "dev", "devb": "devbox", "w": "web"} {
		if got, err := Resolve(ref, names); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	for ref, msg := range map[string]string{"de": "matches several VMs: dev, devbox", "x": "VMs: dev, devbox, web"} {
		if _, err := Resolve(ref, names); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("Resolve(%q) err = %v, want it to mention %q", ref, err, msg)
		}
	}
	if _, err := Resolve("x", nil); err == nil || !strings.Contains(err.Error(), "there are no VMs") {
		t.Errorf("Resolve with no VMs err = %v", err)
	}
}
