package session

import (
	"os"
	"path/filepath"
	"testing"
)

const gnome = "UID=1000\nUSER=alice\nACTIVE=1\nIS_DISPLAY=1\nSTATE=active\nREMOTE=0\nTYPE=wayland\nCLASS=user\nSEAT=seat0\nDISPLAY=:0\n"
const ssh = "UID=1001\nUSER=bob\nACTIVE=1\nSTATE=active\nREMOTE=1\nTYPE=tty\nCLASS=user\n"
const tty = "UID=1002\nUSER=carol\nSTATE=online\nREMOTE=0\nTYPE=tty\nCLASS=user\nSEAT=seat0\n"
const greeter = "UID=120\nUSER=gdm\nSTATE=active\nTYPE=wayland\nCLASS=greeter\n"
const root = "UID=0\nUSER=root\nSTATE=active\nTYPE=tty\nCLASS=user\n"

func write(t *testing.T, dir string, recs map[string]string) {
	t.Helper()
	for id, c := range recs {
		if err := os.WriteFile(filepath.Join(dir, id), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActivePrefersGraphicalThenLocalThenRemote(t *testing.T) {
	for _, tc := range []struct {
		name string
		recs map[string]string
		want string
	}{
		{"graphical beats everything", map[string]string{"1": ssh, "2": tty, "3": gnome}, "alice"},
		{"local tty beats ssh", map[string]string{"1": ssh, "2": tty}, "carol"},
		{"ssh alone", map[string]string{"1": ssh}, "bob"},
		{"greeter and root are ignored", map[string]string{"c1": greeter, "c2": root, "4": ssh}, "bob"},
		{"nobody", map[string]string{"c1": greeter, "c2": root}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.recs)
			os.Symlink("/nonexistent", filepath.Join(dir, "seat0")) // logind keeps per-seat links here
			s, ok := Active(dir)
			if tc.want == "" {
				if ok {
					t.Fatalf("picked %+v", s)
				}
				return
			}
			if !ok || s.User != tc.want {
				t.Fatalf("got %+v ok=%v, want %s", s, ok, tc.want)
			}
		})
	}
}

func TestPickSkipsClosingAndKeepsOldestOnTies(t *testing.T) {
	a, _ := Parse("9", gnome)
	b, _ := Parse("10", gnome)
	b.User = "other"
	c, _ := Parse("2", gnome)
	c.State = "closing"
	c.User = "closing"
	got, ok := Pick([]Session{b, c, a})
	if !ok || got.ID != "9" { // "9" < "10" numerically, not lexically
		t.Fatalf("got %+v", got)
	}
}

func TestActiveMissingDir(t *testing.T) {
	if _, ok := Active(filepath.Join(t.TempDir(), "nope")); ok {
		t.Fatal("found a session in a missing directory")
	}
}

func TestParseRejectsRecordWithoutUID(t *testing.T) {
	if _, err := Parse("1", "USER=x\n"); err == nil {
		t.Fatal("accepted a record without UID")
	}
}
