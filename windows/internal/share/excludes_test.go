package share

import (
	"slices"
	"strings"
	"testing"
)

func excluded(t *testing.T, patterns []string, path string, dir bool) bool {
	t.Helper()
	e, err := CompileExcludes(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return e.Excluded(path, dir)
}

func TestExcludesMatchLikeGitignore(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		path     string
		dir      bool
		want     bool
	}{
		{[]string{"bin/"}, "bin", true, true},
		{[]string{"bin/"}, "src/app/bin", true, true},
		{[]string{"bin/"}, "bin", false, false}, // a file named bin is no directory
		{[]string{"bin/"}, "binary", true, false},
		{[]string{"/root-only"}, "root-only", true, true},
		{[]string{"/root-only"}, "root-only", false, true},
		{[]string{"/root-only"}, "sub/root-only", true, false},
		{[]string{"docs/*.md"}, "docs/a.md", false, true},
		{[]string{"docs/*.md"}, "x/docs/a.md", false, false},
		{[]string{"docs/*.md"}, "docs/sub/a.md", false, false}, // * does not cross /
		{[]string{"*.log"}, "a/b/c.log", false, true},
		{[]string{"*.log"}, "c.log.txt", false, false},
		{[]string{"?.txt"}, "a.txt", false, true},
		{[]string{"?.txt"}, "ab.txt", false, false},
		{[]string{"[ab].txt"}, "b.txt", false, true},
		{[]string{"[ab].txt"}, "c.txt", false, false},
		{[]string{"[!ab].txt"}, "c.txt", false, true},
		{[]string{"[!ab].txt"}, "a.txt", false, false},
		{[]string{"[a-c]x"}, "bx", false, true},
		{[]string{"[a-c]x"}, "dx", false, false},
		{[]string{"a+b(1).txt"}, "a+b(1).txt", false, true}, // regex metacharacters are literal
		{[]string{"**/tmp"}, "tmp", true, true},
		{[]string{"**/tmp"}, "a/b/tmp", true, true},
		{[]string{"a/**/b"}, "a/b", false, true},
		{[]string{"a/**/b"}, "a/x/y/b", false, true},
		{[]string{"a/**/b"}, "x/a/b", false, false},
		{[]string{"logs/**"}, "logs/x/y.txt", false, true},
		{[]string{"logs/**"}, "logs", true, false},
		{[]string{"*.log", "!keep.log"}, "a.log", false, true},
		{[]string{"*.log", "!keep.log"}, "sub/keep.log", false, false},
		{[]string{"!keep.log", "*.log"}, "keep.log", false, true}, // order matters
		{[]string{"a[b"}, "a[b", false, true},                     // an unclosed bracket is literal
	} {
		if got := excluded(t, c.patterns, c.path, c.dir); got != c.want {
			t.Errorf("%q excludes %q (dir %v) = %v, want %v", c.patterns, c.path, c.dir, got, c.want)
		}
	}
}

func TestExcludesSkipCommentsAndBlanksAndHonourEscapes(t *testing.T) {
	e, err := CompileExcludes([]string{"# a comment", "", "   ", `\#hash`, `\!bang`, "trailing   "})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Rules) != 3 {
		t.Fatalf("rules = %v, want 3", e.Rules)
	}
	for _, p := range []string{"#hash", "!bang", "trailing"} {
		if !e.Excluded(p, false) {
			t.Errorf("%q not excluded", p)
		}
	}
}

func TestAPathIsExcludedWhenADirectoryAboveItIs(t *testing.T) {
	e, err := CompileExcludes([]string{"bin/", "*.log", "!important.log"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		dir  bool
		want bool
	}{
		{"src/bin/x/y.dll", false, true},
		{"bin", false, false},
		{"bin", true, true},
		{"bin/important.log", false, true}, // nothing below an excluded directory comes back
		{"src/a.cs", false, false},
	} {
		if got := e.ExcludedOrParent(c.path, c.dir); got != c.want {
			t.Errorf("ExcludedOrParent(%q, %v) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

// The rules must be the ones the macOS edition sends, which the Guest
// Service evaluates with RE2.
func TestRulesAreTheMacOSEditions(t *testing.T) {
	e, err := CompileExcludes([]string{"bin/", "/root-only", "!keep.log", "a/**/b", "[!x]?.c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{
		{Pattern: "^(?:.*/)?bin$", DirOnly: true},
		{Pattern: "^root-only$"},
		{Pattern: `^(?:.*/)?keep\.log$`, Negate: true},
		{Pattern: "^a/(?:.*/)?b$"},
		{Pattern: `^(?:.*/)?[^/x][^/]\.c$`},
	}
	if !slices.Equal(e.Rules, want) {
		t.Errorf("rules = %v, want %v", e.Rules, want)
	}
}

func TestRedundantWildcardsCollapse(t *testing.T) {
	e, err := CompileExcludes([]string{"**/**/**/x", "a/**/**/b", "a***b", "a*****/c"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range e.Rules {
		got = append(got, r.Pattern)
	}
	want := []string{"^(?:.*/)?x$", "^a/(?:.*/)?b$", "^(?:.*/)?a[^/]*b$", "^a[^/]*/c$"}
	if !slices.Equal(got, want) {
		t.Errorf("patterns = %q, want %q", got, want)
	}
	deep := strings.Repeat("a/", 60) + "y"
	big, err := CompileExcludes([]string{strings.Repeat("**/", 30) + "x", "a/" + strings.Repeat("**/", 30) + "x", "a" + strings.Repeat("*", 60) + "b"})
	if err != nil {
		t.Fatal(err)
	}
	if big.ExcludedOrParent(deep, false) || big.ExcludedOrParent(strings.Repeat("a", 200)+"/y", false) {
		t.Error("excluded")
	}
}
