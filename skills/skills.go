// Package skills carries the glov agent skill for glov guide, so a glov
// binary copied on its own prints the text of the build it came from.
package skills

import _ "embed"

// Skill is skills/glov/SKILL.md.
//
//go:embed glov/SKILL.md
var Skill string

// Topics are the skill's references by glov guide topic.
var Topics = map[string]string{
	"troubleshooting": troubleshooting,
}

//go:embed glov/references/troubleshooting.md
var troubleshooting string
