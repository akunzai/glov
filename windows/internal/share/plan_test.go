package share

import (
	"slices"
	"testing"
)

type (
	h = HostFile
	g = GuestFile
)

func set(s ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return m
}

func plan(t *testing.T, host map[string]h, hostDirs map[string]bool, guest map[string]g, guestDirs map[string]bool, exclude, untouched []string) Plan {
	t.Helper()
	e, err := CompileExcludes(exclude)
	if err != nil {
		t.Fatal(err)
	}
	return NewPlan(host, hostDirs, guest, guestDirs, e, untouched)
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func TestPlanWritesNewAndChangedFilesAndDeletesHostDeletions(t *testing.T) {
	p := plan(t, map[string]h{"same": {1, "a"}, "changed": {1, "b"}, "new": {1, "c"}}, nil,
		map[string]g{"same": {1, "a", false}, "changed": {1, "x", false}, "gone": {1, "d", false}}, nil, nil, nil)
	eq(t, "writes", p.Writes, []string{"changed", "new"})
	eq(t, "deletes", p.Deletes, []string{"gone"})
	if p.Unchanged != 1 || len(p.Overwritten) != 0 {
		t.Errorf("plan = %+v", p)
	}
}

func TestPlanTheHashDecidesNotTheSizeAndAnUnknownHashIsAWrite(t *testing.T) {
	p := plan(t, map[string]h{"a": {5, "h1"}, "b": {5, "h2"}}, nil, map[string]g{"a": {5, "h0", false}, "b": {5, "", false}}, nil, nil, nil)
	eq(t, "writes", p.Writes, []string{"a", "b"})
}

func TestPlanLeavesExcludedGuestPathsAlone(t *testing.T) {
	p := plan(t, map[string]h{"src/a.cs": {1, "a"}, "bin/x": {1, "x"}}, set("src", "bin"),
		map[string]g{"bin/z": {1, "z", false}, "root-only/y": {1, "y", false}, "sub/root-only/y": {1, "q", false}},
		set("bin", "root-only", "sub", "sub/root-only"), []string{"bin/", "/root-only"}, nil)
	eq(t, "writes", p.Writes, []string{"src/a.cs"})
	eq(t, "deletes", p.Deletes, []string{"sub/root-only/y"})
	eq(t, "rmdirs", p.Rmdirs, []string{"sub/root-only", "sub"})
	eq(t, "mkdirs", p.Mkdirs, []string{"src"})
}

func TestPlanKeepsTheGuestCopiesOfUntouchedHostPaths(t *testing.T) {
	p := plan(t, nil, nil, map[string]g{"link/f": {1, "f", false}, "link": {1, "l", false}, "other": {1, "o", false}}, nil, nil, []string{"link"})
	eq(t, "deletes", p.Deletes, []string{"other"})
}

func TestPlanCreatesDirectoriesTopDownAndRemovesThemBottomUp(t *testing.T) {
	p := plan(t, map[string]h{"a/b/c/f": {1, "f"}}, set("a", "a/b", "a/b/c"), map[string]g{"x/y/z/g": {1, "g", false}}, set("a", "x", "x/y", "x/y/z"), nil, nil)
	eq(t, "mkdirs", p.Mkdirs, []string{"a/b", "a/b/c"})
	eq(t, "rmdirs", p.Rmdirs, []string{"x/y/z", "x/y", "x"})
}

func TestPlanReplacesAPathThatChangedBetweenFileAndDirectory(t *testing.T) {
	p := plan(t, map[string]h{"was-dir": {1, "f"}, "was-file/inner": {1, "i"}}, set("was-file"),
		map[string]g{"was-file": {1, "w", false}, "was-dir/old": {1, "o", false}}, set("was-dir"), nil, nil)
	eq(t, "deletes", p.Deletes, []string{"was-dir/old", "was-file"})
	eq(t, "rmdirs", p.Rmdirs, []string{"was-dir"})
	eq(t, "mkdirs", p.Mkdirs, []string{"was-file"})
	eq(t, "writes", p.Writes, []string{"was-dir", "was-file/inner"})
}

func TestPlanListsGuestChangesItReplacesOrRemoves(t *testing.T) {
	p := plan(t, map[string]h{"edited": {1, "host"}, "same": {1, "s"}}, nil,
		map[string]g{"edited": {1, "guest", true}, "same": {1, "s", true}, "created": {1, "c", true}}, nil, nil, nil)
	eq(t, "overwritten", p.Overwritten, []string{"created", "edited"}) // an edit that matches the host is no loss
}

func TestParseRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"proj": {"proj", ""}, "proj/": {"proj", ""}, "proj/src/app": {"proj", "src/app"}, `proj\src\app\`: {"proj", "src/app"},
	} {
		if n, s, err := ParseRef(ref); err != nil || n != want[0] || s != want[1] {
			t.Errorf("ParseRef(%q) = %q, %q, %v", ref, n, s, err)
		}
	}
	for _, ref := range []string{"", "/x", ".git", "proj//a", "proj/../x", "proj/./x", "proj/c:x"} {
		if _, _, err := ParseRef(ref); err == nil {
			t.Errorf("ParseRef(%q) accepted", ref)
		}
	}
}
