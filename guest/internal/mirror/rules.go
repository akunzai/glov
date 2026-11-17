package mirror

import (
	"regexp"

	"github.com/akunzai/glov/guest/proto"
)

// Rule is one exclude rule as the host sends it: an RE2 expression matched
// against a whole relative path (the host compiles gitignore patterns into
// these, so the Guest Service needs no gitignore parser of its own).
type Rule struct {
	Pattern string `json:"pattern"`
	Negate  bool   `json:"negate,omitzero"`
	DirOnly bool   `json:"dirOnly,omitzero"`
}

type compiledRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

// Rules decides which paths a sync leaves alone.
type Rules []compiledRule

// CompileRules compiles the host's rules, or reports the first bad one as
// invalid params.
func CompileRules(rules []Rule) (Rules, error) {
	out := make(Rules, 0, len(rules))
	for _, r := range rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, proto.Errorf(proto.CodeInvalidParams, "exclude pattern %q: %v", r.Pattern, err)
		}
		out = append(out, compiledRule{re, r.Negate, r.DirOnly})
	}
	return out, nil
}

// Excluded reports whether path (relative, '/'-separated) is excluded: the
// last rule that matches it decides. A caller walking a tree does not descend
// into an excluded directory, so nothing below it can be re-included, as in
// gitignore.
func (rs Rules) Excluded(path string, dir bool) bool {
	excluded := false
	for _, r := range rs {
		if r.dirOnly && !dir {
			continue
		}
		if r.re.MatchString(path) {
			excluded = !r.negate
		}
	}
	return excluded
}
