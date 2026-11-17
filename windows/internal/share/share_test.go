package share

import (
	"strings"
	"testing"
)

func TestGuestPathIsBelowMntWithALowercaseDrive(t *testing.T) {
	for host, want := range map[string]string{
		`C:\src\Proj`:     "/mnt/c/src/Proj",
		`d:\a b\c.d\`:     "/mnt/d/a b/c.d",
		`E:\x\Ünïcode\長い`: "/mnt/e/x/Ünïcode/長い",
	} {
		if got, err := GuestPath(host); err != nil || got != want {
			t.Errorf("GuestPath(%q) = %q, %v; want %q", host, got, err, want)
		}
	}
	for _, host := range []string{`C:\`, `C:`, `C:x`, `\\server\share\x`, `\\?\C:\x`, `C:\a\..\b`, `C:\a\.\b`, `C:\a\\b`, `1:\x`, `/mnt/c/x`, `C:\a/b`} {
		if got, err := GuestPath(host); err == nil {
			t.Errorf("GuestPath(%q) = %q, want an error", host, got)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"proj", "My.Proj_2-x", strings.Repeat("a", MaxNameLen), "console"} {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", ".git", "a/b", `a\b`, "a b", "a:b", strings.Repeat("a", MaxNameLen+1), "CON", "nul.txt", "com1", "Lpt9.x"} {
		if ValidName(n) == nil {
			t.Errorf("ValidName(%q) accepted", n)
		}
	}
}

func TestCheckRefusesProtectedAndOverlappingFolders(t *testing.T) {
	protected := []Protected{
		{Path: `C:\Users\me`, Why: "your whole user profile"},
		{Path: `C:\Users\me\AppData\Local\glov`, Tree: true, Why: "glov's folder"},
		{Path: `C:\Users\me\.ssh`, Tree: true, Why: "a folder of credentials"},
	}
	existing := []Share{{Name: "a", HostPath: `C:\src\a`, Mode: RW}}
	ok := func(host string) {
		t.Helper()
		s, err := Check(Share{Name: "b", HostPath: host}, existing, protected)
		if err != nil || s.Mode != RW {
			t.Errorf("Check(%s) = %+v, %v; want a rw share", host, s, err)
		}
	}
	refused := func(name, host, why string) {
		t.Helper()
		if _, err := Check(Share{Name: name, HostPath: host}, existing, protected); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("Check(%s, %s) = %v, want an error about %q", name, host, err, why)
		}
	}
	ok(`C:\Users\me\src\proj`)
	ok(`C:\src\ab`)
	refused("b", `c:\users\ME`, "your whole user profile")
	refused("b", `C:\Users`, "glov's folder")
	refused("b", `C:\Users\me\AppData`, "glov's folder")
	refused("b", `C:\Users\me\AppData\Local\glov\vms\dev`, "glov's folder")
	refused("b", `C:\Users\me\.ssh\keys`, "credentials")
	refused("b", `C:\`, "drive root")
	refused("b", `C:\SRC\A\inner`, "overlaps share a")
	refused("b", `C:\src`, "overlaps share a")
	refused("a", `D:\other`, "already has a share named a")
	refused("-bad name", `D:\other`, "invalid share name")
	if _, err := Check(Share{Name: "b", HostPath: `D:\x`, Exclude: []string{"bin/"}, Mode: "rx"}, nil, nil); err == nil {
		t.Error("mode rx accepted")
	}
}

func TestAllowedHonoursModesAndExtraFolders(t *testing.T) {
	shares := []Share{{Name: "ro", HostPath: `C:\ref`, Mode: RO}, {Name: "rw", HostPath: `C:\src\proj`, Mode: RW}}
	extra := []string{`C:\Users\me\AppData\Local\glov\scratch`}
	for _, c := range []struct {
		path  string
		write bool
		want  bool
	}{
		{`C:\ref\a.txt`, false, true},
		{`C:\ref\a.txt`, true, false},
		{`C:\REF`, false, true},
		{`C:\reference\a.txt`, false, false},
		{`C:\src\proj\out\x`, true, true},
		{`c:\users\me\appdata\local\glov\scratch\x`, true, true},
		{`C:\Users\me\AppData\Local\glov\secrets`, false, false},
		{`C:\src`, false, false},
	} {
		if got := Allowed(c.path, shares, extra, c.write); got != c.want {
			t.Errorf("Allowed(%s, write %v) = %v, want %v", c.path, c.write, got, c.want)
		}
	}
}

func TestFindListsTheShares(t *testing.T) {
	if _, err := Find(nil, "x"); err == nil || !strings.Contains(err.Error(), "no shares") {
		t.Errorf("Find = %v", err)
	}
	if _, err := Find([]Share{{Name: "a"}, {Name: "b"}}, "x"); err == nil || !strings.Contains(err.Error(), "shares: a, b") {
		t.Errorf("Find = %v", err)
	}
}

func TestAShareMayNameItsOwnGuestPath(t *testing.T) {
	s, err := Check(Share{Name: "harness-sessions", HostPath: `C:\Users\me\AppData\Local\glov\sessions\box`, Mode: Live, Guest: "/mnt/glov-sessions"}, nil, nil)
	if err != nil || s.GuestPath() != "/mnt/glov-sessions" {
		t.Fatalf("Check = %+v (%s), %v", s, s.GuestPath(), err)
	}
	for _, bad := range []string{"/srv/x", "/mnt/a/../b", "/mnt/a/", `/mnt/a\b`, "mnt/a"} {
		if _, err := Check(Share{Name: "x", HostPath: `C:\src\x`, Guest: bad}, nil, nil); err == nil {
			t.Errorf("guest path %q accepted", bad)
		}
	}
	if _, err := Check(Share{Name: "y", HostPath: `C:\src\y`, Guest: "/mnt/glov-sessions/y"}, []Share{s}, nil); err == nil {
		t.Error("a share inside another's guest path was accepted")
	}
	if _, err := Check(Share{Name: "z", HostPath: `C:\mnt\z`}, []Share{{Name: "w", HostPath: `D:\w`, Guest: "/mnt/c/mnt"}}, nil); err == nil {
		t.Error("a share below another's named guest path was accepted")
	}
}
