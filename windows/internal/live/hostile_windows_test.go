package live

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akunzai/glov/windows/internal/share"
	"golang.org/x/sys/windows"
)

// hostileFixture is a share root with a world outside it a hostile guest
// must not reach: a file and a directory beside the root, and a junction
// (plus symbolic links, when the user may make them) inside the root that
// point out to them.
type hostileFixture struct {
	parent, dir, outdir, outfile string
	root                         *os.Root
	host                         *Host
	// token names what a hostile path tries to create outside, so
	// anything by that name in a watched directory is an escape.
	token string
	links bool // the symbolic links could be made
	// short is the 8.3 name of the junction "longjunction" (to outdir),
	// or "" when the volume makes none.
	short string
}

func newHostileFixture(t *testing.T) *hostileFixture {
	t.Helper()
	f := &hostileFixture{parent: t.TempDir(), token: "glov-escape-" + strings.ToLower(rand.Text()[:10])}
	f.dir = filepath.Join(f.parent, "share")
	f.outdir = filepath.Join(f.parent, "outside")
	f.outfile = filepath.Join(f.parent, "outside.txt")
	for _, d := range []string{f.dir, f.outdir, filepath.Join(f.outdir, "sub")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range map[string]string{f.outfile: "outside", filepath.Join(f.outdir, "victim.txt"): "victim"} {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(f.dir, "j"), f.outdir).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, b)
	}
	long := filepath.Join(f.dir, "longjunction")
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", long, f.outdir).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, b)
	}
	if s, err := shortName(long); err == nil && !strings.EqualFold(s, "longjunction") {
		f.short = s
	}
	f.links = os.Symlink(f.outdir, filepath.Join(f.dir, "s")) == nil &&
		os.Symlink(f.outfile, filepath.Join(f.dir, "fs")) == nil
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	f.root, f.host = root, NewHost(root, share.Excludes{}, nil)
	return f
}

// outside is everything outside the share root that a test watches, by
// path: a file's content, "dir", or a link's target.
func (f *hostileFixture) outside(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(f.parent, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == f.dir {
			return filepath.SkipDir // the root itself is the guest's to change
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0 || d.Type()&fs.ModeIrregular != 0:
			target, _ := os.Readlink(p)
			out[p] = "link " + target
		case d.IsDir():
			out[p] = "dir"
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// What a drive-relative, rooted or absolute name would reach.
	wd, _ := os.Getwd()
	for _, d := range []string{wd, `C:\`, filepath.VolumeName(f.parent) + `\`, os.TempDir()} {
		if _, err := os.Lstat(filepath.Join(d, f.token)); err == nil {
			out[filepath.Join(d, f.token)] = "escaped"
		}
	}
	return out
}

// sameOutside fails t if the world outside the root changed since before.
func (f *hostileFixture) sameOutside(t *testing.T, before map[string]string, what string) {
	t.Helper()
	after := f.outside(t)
	for _, p := range slices.Sorted(maps.Keys(after)) {
		if b, ok := before[p]; !ok || b != after[p] {
			t.Errorf("%s changed %s outside the root: %q, was %q", what, p, after[p], b)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("%s deleted %s outside the root", what, p)
		}
	}
}

// hostileNames are guest paths that try to leave the root, by class.
func hostileNames(f *hostileFixture) map[string][]string {
	tok := f.token
	abs := filepath.Join(f.parent, tok)
	return map[string][]string{
		"dot-dot":           {"../" + tok, "a/../../" + tok, "../outside.txt", "../outside/victim.txt", "..", "a/.."},
		"backslash":         {`..\` + tok, `a\..\..\` + tok, `..\outside.txt`, `..\outside\victim.txt`, `a\b\..\..\..\` + tok},
		"drive-relative":    {"C:" + tok, filepath.VolumeName(f.parent) + tok, "C:../" + tok},
		"rooted":            {"/" + tok, `\` + tok, "//localhost/C$/" + tok},
		"absolute":          {abs, filepath.ToSlash(abs), filepath.Join(f.outdir, "victim.txt"), `\\?\` + abs, `\\.\` + abs, `\\localhost\C$\` + tok},
		"stream":            {"x:stream", "x::$DATA", ".:stream", "../outside.txt:stream", "j:stream", "j/victim.txt:stream"},
		"device":            {"NUL", "CON", "COM1", "LPT1", "AUX", "PRN", "CONIN$", "CONOUT$", "nul.txt", "a/NUL", "NUL ", "COM¹"},
		"trailing":          {".. /" + tok, "../ /" + tok, "... /" + tok, "../." + tok, ". ./" + tok},
		"through-junction":  {"j/" + tok, "j/victim.txt", "j/sub", "j/sub/" + tok, "J/" + tok, "j./" + tok, "j /" + tok},
		"through-symlink":   {"s/" + tok, "s/victim.txt", "s/sub", "fs", "s./" + tok, "s /" + tok},
		"short-name":        {f.short + "/" + tok, f.short + "/victim.txt", f.short + "/sub", f.short + "/sub/" + tok},
		"wildcard-question": {"?/" + tok, `\\?\..\` + tok, "*", "a?b", "<", ">", "|", `"`},
	}
}

// TestAHostileGuestNameNeverReachesOutsideTheRoot drives each host change a
// guest's name can cause (Put, Mkdir, Remove, Rmdir) with each hostile name
// class against a real os.Root, and checks nothing outside it changed.
func TestAHostileGuestNameNeverReachesOutsideTheRoot(t *testing.T) {
	names := hostileNames(newHostileFixture(t))
	for _, class := range slices.Sorted(maps.Keys(names)) {
		for i := range names[class] {
			f := newHostileFixture(t)
			p := hostileNames(f)[class][i]
			if class == "through-symlink" && !f.links {
				t.Logf("%s %q: skipped, symbolic links need Developer Mode or the privilege", class, p)
				continue
			}
			if class == "short-name" && f.short == "" {
				t.Logf("%s %q: skipped, the volume makes no 8.3 names", class, p)
				continue
			}
			before := f.outside(t)
			content := "from the guest"
			for _, op := range []struct {
				name string
				do   func() error
			}{
				{"Put", func() error { return f.host.Put(p, strings.NewReader(content), sum(content), "") }},
				{"Put/victim", func() error { return f.host.Put(p, strings.NewReader(content), sum(content), sum("victim")) }},
				{"Put/out", func() error { return f.host.Put(p, strings.NewReader(content), sum(content), sum("outside")) }},
				{"Mkdir", func() error { return f.host.Mkdir(p) }},
				{"Remove", func() error { return f.host.Remove(p, sum("victim")) }},
				{"Remove/out", func() error { return f.host.Remove(p, sum("outside")) }},
				{"Rmdir", func() error { return f.host.Rmdir(p) }},
			} {
				err := op.do()
				t.Logf("%s %s %q: %v", class, op.name, p, err)
				f.sameOutside(t, before, class+" "+op.name+" "+p)
			}
		}
	}
}

// TestAHostileGuestsNamesAreRefusedOnTheHost runs rounds against a guest
// whose manifest and change notifications carry names no Windows folder
// should take: each is refused before the host touches its file system,
// nothing appears in the root under another name, and the guest's other
// files still arrive.
func TestAHostileGuestsNamesAreRefusedOnTheHost(t *testing.T) {
	f := newHostileFixture(t)
	guest := newSide("guest")
	bad := []string{
		"../" + f.token, `..\` + f.token, "a/../" + f.token, "C:" + f.token, "/" + f.token, `\` + f.token,
		"x.", "y ", "d./f", "e /f", `a\b`, "x:stream", "NUL", "con.txt", "COM1.log", "lpt9", "CONIN$",
		"j /f", "q?", "q*", "q<", "q|", "q\x01", "./g", "h//i",
	}
	for _, p := range bad {
		guest.files[p] = "from the guest"
	}
	guest.dirs[".. "] = true
	guest.dirs["k."] = true
	bad = append(bad, ".. ", "k.")
	guest.files["ok/record.jsonl"] = "a record"
	guest.dirs["ok"] = true
	e := &Engine{Host: f.host, Guest: guest}
	before := f.outside(t)
	for _, full := range []bool{true, false} {
		r, err := e.Round(full, bad)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range bad {
			// A change notification's path is cleaned first ("./g" is
			// "g", "../x" dropped), so a round may never meet p at all.
			if err := r.Retry[p]; !errors.Is(err, ErrName) && (full || err != nil) {
				t.Errorf("full %v: %q: %v, want it refused", full, p, r.Retry[p])
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(f.dir, "ok", "record.jsonl")); err != nil || string(b) != "a record" {
		t.Errorf("ok/record.jsonl: %q %v", b, err)
	}
	var names []string
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range entries {
		names = append(names, d.Name())
	}
	want := []string{"j", "longjunction", "ok"}
	if f.links {
		want = []string{"fs", "j", "longjunction", "ok", "s"}
	}
	if !slices.Equal(names, want) {
		t.Errorf("the root holds %q, want %q", names, want)
	}
	f.sameOutside(t, before, "a round")
}

// TestAGuestsChangesUnderExcludedPathsAreRefusedOnTheHost runs rounds
// against a guest that ignores the share's excludes, then makes each change
// directly: every one under an excluded file or directory is refused before
// the host touches its file system, nothing new appears on the host, an
// excluded file already there stays, and the guest's other files arrive.
func TestAGuestsChangesUnderExcludedPathsAreRefusedOnTheHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret", "kept.txt"), []byte("host only"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ex, err := share.CompileExcludes([]string{"secret/", "*.key", "build/out"})
	if err != nil {
		t.Fatal(err)
	}
	host := NewHost(root, ex, nil)
	guest := newSide("guest")
	bad := []string{"secret/x", "secret/sub/y", "a.key", "src/b.key", "build/out", "build/out/z", "fresh/../secret/w"}
	for _, p := range bad {
		guest.files[p] = "from the guest"
	}
	guest.dirs["secret"] = true
	guest.dirs["secret/d"] = true
	guest.files["src/main.go"] = "package main"
	guest.dirs["src"] = true
	e := &Engine{Host: host, Guest: guest}
	for _, full := range []bool{true, false} {
		r, err := e.Round(full, append(bad, "secret/d"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"secret/x", "secret/sub/y", "a.key", "src/b.key", "build/out/z", "secret/d"} {
			if err := r.Retry[p]; !errors.Is(err, ErrExcluded) {
				t.Errorf("full %v: %q: %v, want it refused as excluded", full, p, err)
			}
		}
	}
	for _, op := range []struct {
		name string
		do   func() error
	}{
		{"Put", func() error { return host.Put("secret/new", strings.NewReader("x"), sum("x"), "") }},
		{"Put/kept", func() error { return host.Put("secret/kept.txt", strings.NewReader("x"), sum("x"), sum("host only")) }},
		{"Mkdir", func() error { return host.Mkdir("secret/newdir") }},
		{"Mkdir/excluded", func() error { return host.Mkdir("build/out") }},
		{"Remove", func() error { return host.Remove("secret/kept.txt", sum("host only")) }},
		{"Rmdir", func() error { return host.Rmdir("secret") }},
	} {
		if err := op.do(); !errors.Is(err, ErrExcluded) {
			t.Errorf("%s: %v, want ErrExcluded", op.name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "secret", "kept.txt")); err != nil || string(b) != "host only" {
		t.Errorf("secret/kept.txt: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "src", "main.go")); err != nil || string(b) != "package main" {
		t.Errorf("src/main.go: %q %v", b, err)
	}
	var got []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	if want := []string{"secret", "secret/kept.txt", "src", "src/main.go"}; !slices.Equal(got, want) {
		t.Errorf("the root holds %q, want %q", got, want)
	}
}

// TestAnExcludedPathSpelledAnotherWayIsStillRefused names excluded paths
// the way NTFS also finds them, in another case or by an 8.3 short name,
// and by a type other than the one on disk: the host neither changes nor
// reads them for the guest.
func TestAnExcludedPathSpelledAnotherWayIsStillRefused(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"secret", "credentials-store", "x.cfg"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range map[string]string{"secret/kept.txt": "host only", "credentials-store/token": "host only", "y.cfg": "host only"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// *.cfg files are excluded, x.cfg/ as a directory is not.
	ex, err := share.CompileExcludes([]string{"secret/", "credentials-store/", "*.cfg", "!*.cfg/", "vault/"})
	if err != nil {
		t.Fatal(err)
	}
	host := NewHost(root, ex, nil)
	names := []string{"SECRET", "Secret", "sEcReT"}
	if s, err := shortName(filepath.Join(dir, "credentials-store")); err == nil && !strings.EqualFold(s, "credentials-store") {
		names = append(names, s)
	} else {
		t.Logf("8.3: skipped, the volume makes no short names")
	}
	names = append(names, "CREDENTIALS-STORE")
	for _, n := range names {
		file := n + "/kept.txt"
		if strings.HasPrefix(strings.ToLower(n), "cred") || strings.Contains(n, "~") {
			file = n + "/token"
		}
		for _, op := range []struct {
			name string
			do   func() error
		}{
			{"Put", func() error { return host.Put(n+"/new", strings.NewReader("x"), sum("x"), "") }},
			{"Put/existing", func() error { return host.Put(file, strings.NewReader("x"), sum("x"), sum("host only")) }},
			{"Remove", func() error { return host.Remove(file, sum("host only")) }},
			{"Mkdir", func() error { return host.Mkdir(n + "/d") }},
			{"Rmdir", func() error { return host.Rmdir(n) }},
			{"Open", func() error {
				r, err := host.Open(file)
				if err == nil {
					r.Close()
				}
				return err
			}},
		} {
			if err := op.do(); !errors.Is(err, ErrExcluded) {
				t.Errorf("%s %q: %v, want ErrExcluded", op.name, n, err)
			}
		}
		if st, ok := host.stat(file); ok {
			t.Errorf("stat %q: %+v, want it absent", file, st)
		}
	}
	// The guest claims y.cfg is a directory (not excluded as one); on disk it
	// is an excluded file.
	if err := host.Rmdir("y.cfg"); !errors.Is(err, ErrExcluded) {
		t.Errorf("Rmdir y.cfg: %v, want ErrExcluded", err)
	}
	if err := host.Remove("Y.CFG", "dir"); !errors.Is(err, ErrExcluded) {
		t.Errorf("Remove Y.CFG: %v, want ErrExcluded", err)
	}
	// A folder the guest would make in another case, where Windows'
	// programs would then write secret	oken, is excluded too, and one
	// already there is not listed.
	if err := host.Mkdir("Vault"); !errors.Is(err, ErrExcluded) {
		t.Errorf("Mkdir Vault: %v, want ErrExcluded", err)
	}
	if err := host.Put("VAULT/token", strings.NewReader("x"), sum("x"), ""); !errors.Is(err, ErrExcluded) {
		t.Errorf("Put VAULT/token: %v, want ErrExcluded", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Other", "VAULT"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Other", "VAULT", "token"), []byte("host only"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := host.List("")
	if err != nil {
		t.Fatal(err)
	}
	for p := range snap {
		if strings.Contains(strings.ToLower(p), "vault") {
			t.Errorf("List has %q", p)
		}
	}
	// x.cfg/ is a directory the share keeps: its changes go through.
	if err := host.Put("x.cfg/in", strings.NewReader("x"), sum("x"), ""); err != nil {
		t.Errorf("Put x.cfg/in: %v", err)
	}
	for p, want := range map[string]string{"secret/kept.txt": "host only", "credentials-store/token": "host only", "y.cfg": "host only", "x.cfg/in": "x"} {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p))); err != nil || string(b) != want {
			t.Errorf("%s: %q %v, want %q", p, b, err, want)
		}
	}
	for _, d := range []string{"secret", "credentials-store"} {
		if entries, _ := os.ReadDir(filepath.Join(dir, d)); len(entries) != 1 {
			t.Errorf("%s holds %d entries, want 1", d, len(entries))
		}
	}
}

// TestAJunctionSwappedInMidChangeIsNotFollowed swaps a directory of the root
// for a junction out of it and back, again and again, while the host makes
// changes below it: whichever the host meets, nothing outside changes.
func TestAJunctionSwappedInMidChangeIsNotFollowed(t *testing.T) {
	f := newHostileFixture(t)
	d := filepath.Join(f.dir, "d")
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "victim.txt"), []byte("victim"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The junction waits beside d, so swapping is two renames, with little
	// time between them for the host to make d again.
	dj := filepath.Join(f.dir, "dj")
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", dj, f.outdir).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, b)
	}
	before := f.outside(t)
	// rename tries from to to up to tries times, clearing a plain directory
	// the host made at to meanwhile (inside the root).
	rename := func(from, to string, tries int) error {
		var err error
		for range tries {
			if fi, lerr := os.Lstat(to); lerr == nil && fi.IsDir() && fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
				os.RemoveAll(to)
			}
			if err = os.Rename(from, to); err == nil {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		return err
	}
	done := make(chan struct{})
	swaps := 0
	go func() {
		defer close(done)
		for i := range 50 {
			// A name of its own each time: one the host still holds open
			// cannot be put back, nor removed at once.
			moved := filepath.Join(f.dir, "d.moved"+strconv.Itoa(i))
			if os.Rename(d, moved) != nil {
				continue // the host holds a file in it open
			}
			// The host makes d again as soon as it writes there, and
			// clearing that d races its next write: give up on this swap
			// soon rather than fight the host for seconds.
			if err := rename(dj, d, 20); err == nil {
				swaps++
				time.Sleep(time.Millisecond)
				if err := rename(d, dj, 200); err != nil {
					t.Errorf("moving the junction back: %v", err)
					return
				}
			}
			if os.Rename(moved, d) == nil {
				continue
			}
			// The host may have made d again, a plain folder inside the
			// root it keeps writing to: go on with that d rather than
			// race the host for it.
			if fi, err := os.Lstat(d); err == nil && fi.IsDir() && fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
				os.RemoveAll(moved)
				continue
			}
			if err := rename(moved, d, 200); err != nil {
				t.Errorf("putting d back: %v", err)
				return
			}
		}
	}()
	content := "from the guest"
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		f.host.Put("d/"+f.token, strings.NewReader(content), sum(content), "")
		f.host.Put("d/victim.txt", strings.NewReader(content), sum(content), sum("victim"))
		f.host.Remove("d/victim.txt", sum("victim"))
		f.host.Remove("d/"+f.token, sum(content))
		f.host.Mkdir("d/sub/" + f.token)
		f.host.Rmdir("d/sub/" + f.token)
		f.host.Rmdir("d/sub")
	}
	if swaps == 0 {
		t.Skip("the junction was never swapped in")
	}
	t.Logf("%d swaps", swaps)
	f.sameOutside(t, before, "a swapped junction")
}

// shortName is the 8.3 name of p's last element.
func shortName(p string) (string, error) {
	long, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil {
		return "", err
	}
	return filepath.Base(windows.UTF16ToString(buf[:n])), nil
}
