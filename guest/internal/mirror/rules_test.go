package mirror_test

import (
	"testing"

	"github.com/akunzai/glov/guest/internal/mirror"
)

// The host compiles gitignore patterns into these expressions
// (Sources/GlovCore/ShareExcludes.swift, ShareExcludesTests checks the same
// strings); RE2 must read them as ICU does.
func TestHostCompiledRulesMatchAsOnTheHost(t *testing.T) {
	rules, err := mirror.CompileRules([]mirror.Rule{
		{Pattern: `^(?:.*/)?bin$`, DirOnly: true},
		{Pattern: `^root-only$`},
		{Pattern: `^(?:.*/)?[^/]*\.log$`},
		{Pattern: `^(?:.*/)?keep\.log$`, Negate: true},
		{Pattern: `^a/(?:.*/)?b$`},
		{Pattern: `^(?:.*/)?[^/x][^/]\.c$`},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		dir  bool
		want bool
	}{
		{"bin", true, true}, {"src/bin", true, true}, {"bin", false, false}, {"binary", true, false},
		{"root-only", true, true}, {"sub/root-only", true, false},
		{"x/y.log", false, true}, {"x/keep.log", false, false},
		{"a/b", false, true}, {"a/x/y/b", false, true}, {"x/a/b", false, false},
		{"ab.c", false, true}, {"xb.c", false, false}, {"d/ab.c", false, true},
	} {
		if got := rules.Excluded(c.path, c.dir); got != c.want {
			t.Errorf("Excluded(%q, dir %v) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}
