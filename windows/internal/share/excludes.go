package share

import (
	"fmt"
	"regexp"
	"strings"
)

// Rule is one exclude rule as the Guest Service takes it in sync.manifest: an
// RE2 expression matched against a whole relative path
// (docs/guest-protocol.md, "Share sync").
type Rule struct {
	Pattern string `json:"pattern"`
	Negate  bool   `json:"negate,omitzero"`
	DirOnly bool   `json:"dirOnly,omitzero"`
}

// Excludes are a share's exclude patterns in gitignore syntax, relative to
// the share root: '#' starts a comment, '!' re-includes, a trailing '/'
// matches directories only, a '/' at the start or in the middle anchors the
// pattern to the share root (otherwise it matches at any depth), '*', '?' and
// '[...]' never match '/', and '**' matches across directories as a leading
// '**/', a trailing '/**' or an inner '/**/'. The last matching pattern
// decides, and nothing below an excluded directory is seen, so it cannot be
// re-included. It is the macOS edition's ShareExcludes
// (Sources/GlovCore/ShareExcludes.swift), so both compile a pattern to
// the same rule.
type Excludes struct {
	Rules []Rule
	res   []*regexp.Regexp
}

// CompileExcludes compiles patterns, or names the first it cannot read.
func CompileExcludes(patterns []string) (Excludes, error) {
	var e Excludes
	for _, p := range patterns {
		r, ok := compile(p)
		if !ok {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return Excludes{}, fmt.Errorf("cannot read exclude pattern %q", p)
		}
		e.Rules = append(e.Rules, r)
		e.res = append(e.res, re)
	}
	return e, nil
}

// IgnoringCase is e matching names in any case, as a Windows folder finds
// them; its Rules, which the Guest Service takes, stay as they are.
func (e Excludes) IgnoringCase() Excludes {
	out := Excludes{Rules: e.Rules}
	for _, r := range e.Rules {
		out.res = append(out.res, regexp.MustCompile("(?i)"+r.Pattern))
	}
	return out
}

// Excluded reports whether path (relative, '/'-separated) itself is excluded.
func (e Excludes) Excluded(path string, dir bool) bool {
	excluded := false
	for i, r := range e.Rules {
		if (!r.DirOnly || dir) && e.res[i].MatchString(path) {
			excluded = !r.Negate
		}
	}
	return excluded
}

// ExcludedOrParent reports whether path or a directory above it is excluded.
func (e Excludes) ExcludedOrParent(path string, dir bool) bool {
	for i := range len(path) {
		if path[i] == '/' && e.Excluded(path[:i], true) {
			return true
		}
	}
	return e.Excluded(path, dir)
}

const (
	anyDirs = "(?:.*/)?"
	anyName = "[^/]*"
)

func compile(raw string) (Rule, bool) {
	p := raw
	// Trailing spaces go unless escaped.
	for strings.HasSuffix(p, " ") && !strings.HasSuffix(p, `\ `) {
		p = p[:len(p)-1]
	}
	if p == "" || strings.HasPrefix(p, "#") {
		return Rule{}, false
	}
	negate := strings.HasPrefix(p, "!")
	p = strings.TrimPrefix(p, "!")
	dirOnly := false
	for strings.HasSuffix(p, "/") {
		dirOnly, p = true, p[:len(p)-1]
	}
	if p == "" {
		return Rule{}, false
	}
	anchored := strings.Contains(p[:len(p)-1], "/")
	p = strings.TrimPrefix(p, "/")
	chars := []rune(p)
	var out strings.Builder
	if !anchored {
		out.WriteString(anyDirs)
	}
	// Repeated wildcards are collapsed as they are emitted ("**/**/" is
	// "**/", "***" and "**" inside a name are "*"), as on macOS, where each
	// copy multiplies a backtracking matcher's work.
	emit := func(part string) {
		if (part != anyDirs && part != anyName) || !strings.HasSuffix(out.String(), part) {
			out.WriteString(part)
		}
	}
	for i := 0; i < len(chars); {
		c := chars[i]
		switch c {
		case '*':
			end := i
			for end < len(chars) && chars[end] == '*' {
				end++
			}
			atStart, afterSlash := i == 0, i > 0 && chars[i-1] == '/'
			atEnd, beforeSlash := end == len(chars), end < len(chars) && chars[end] == '/'
			switch {
			case end-i == 2 && (atStart || afterSlash) && beforeSlash:
				emit(anyDirs) // "**/" at the start, or "/**/": zero or more directories
				i = end + 1
			case end-i == 2 && afterSlash && atEnd:
				out.WriteString(".*") // "/**": everything inside
				i = end
			default:
				emit(anyName)
				i = end
			}
			continue
		case '?':
			out.WriteString("[^/]")
		case '[':
			if cls, next, ok := bracket(chars, i); ok {
				out.WriteString(cls)
				i = next
				continue
			}
			out.WriteString(`\[`)
		case '\\':
			if i+1 < len(chars) {
				i++
				out.WriteString(literal(chars[i]))
			} else {
				out.WriteString(`\\`)
			}
		default:
			out.WriteString(literal(c))
		}
		i++
	}
	return Rule{Pattern: "^" + out.String() + "$", Negate: negate, DirOnly: dirOnly}, true
}

func literal(c rune) string {
	if strings.ContainsRune(`.^$|?*+()[]{}\`, c) {
		return `\` + string(c)
	}
	return string(c)
}

// bracket reads "[...]" starting at start as a class that never matches '/',
// and returns the index after it; ok is false when it is unclosed.
func bracket(chars []rune, start int) (string, int, bool) {
	i := start + 1
	negated := false
	if i < len(chars) && (chars[i] == '!' || chars[i] == '^') {
		negated = true
		i++
	}
	var body strings.Builder
	for first := true; i < len(chars); i++ {
		c := chars[i]
		if c == ']' && !first {
			head := "["
			if negated {
				head += "^/"
			}
			return head + body.String() + "]", i + 1, true
		}
		first = false
		switch c {
		case '/': // a class never matches the separator
		case '\\':
			if i+1 < len(chars) {
				i++
				body.WriteString(`\` + string(chars[i]))
			}
		case '[', ']', '^', '&', '~':
			body.WriteString(`\` + string(c))
		default:
			body.WriteRune(c)
		}
	}
	return "", 0, false
}
