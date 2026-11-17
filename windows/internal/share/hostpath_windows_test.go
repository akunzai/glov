package share

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// realTemp is t.TempDir() as Windows names it (the short 8.3 names of a
// TEMP under the profile expanded).
func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := finalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCanonicalNamesTheFolderAsWindowsStoresIt(t *testing.T) {
	root := realTemp(t)
	proj := filepath.Join(root, "Proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Canonical(strings.ToUpper(proj) + `\.`)
	if err != nil || got != proj {
		t.Fatalf("Canonical = %q, %v; want %q", got, err, proj)
	}
	long := filepath.Join(root, strings.Repeat("d", 120), strings.Repeat("e", 120), strings.Repeat("f", 120))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := Canonical(long); err != nil || got != long {
		t.Fatalf("Canonical(long) = %q, %v", got, err)
	}
	if _, err := Canonical(filepath.Join(root, "missing")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing: %v", err)
	}
	file := filepath.Join(root, "file")
	os.WriteFile(file, nil, 0o644)
	if _, err := Canonical(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file: %v", err)
	}
}

func TestCanonicalRefusesJunctionsAndLinksOnTheWay(t *testing.T) {
	root := realTemp(t)
	target := filepath.Join(root, "target")
	os.MkdirAll(filepath.Join(target, "inner"), 0o755)
	junction := filepath.Join(root, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	for _, p := range []string{junction, filepath.Join(junction, "inner")} {
		if _, err := Canonical(p); err == nil || !strings.Contains(err.Error(), "symbolic link or junction") {
			t.Errorf("Canonical(%s) = %v, want a refusal", p, err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Logf("no symbolic link (needs Developer Mode or elevation): %v", err)
	} else if _, err := Canonical(link); err == nil || !strings.Contains(err.Error(), "symbolic link or junction") {
		t.Errorf("Canonical(link) = %v, want a refusal", err)
	}
	// Resolve, for the copy allowlist, follows it to where a copy would go.
	if got, err := Resolve(filepath.Join(junction, "inner", "new", "file")); err != nil || got != filepath.Join(target, "inner", "new", "file") {
		t.Errorf("Resolve = %q, %v", got, err)
	}
}

func TestResolveRefusesUNCAndDevicePathsWithoutTouchingThem(t *testing.T) {
	for _, p := range []string{`\\host.invalid\share\x`, `//host.invalid/share/x`, `\\?\UNC\host.invalid\share\x`, `\\.\pipe\x`, `\\?\C:\x`} {
		if _, err := Resolve(p); err == nil || !strings.Contains(err.Error(), "not on a drive") {
			t.Errorf("Resolve(%s) = %v", p, err)
		}
	}
	if _, err := Resolve(t.TempDir()); err != nil {
		t.Errorf("a local folder: %v", err)
	}
}
