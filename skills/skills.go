// Package skills embeds the skills aggy links into the vendor CLIs, so
// the binary carries them and aggy skill link can install them without
// a source checkout.
package skills

import (
	"bytes"
	_ "embed"
)

// Aggy is skills/aggy/SKILL.md, the aggy skill.
//
//go:embed aggy/SKILL.md
var Aggy []byte

// Estimate returns a skill's estimated token cost, offline, as bytes / 4
// rounded up (a common rule of thumb for English text; no tokenizer, no
// model call): always is the frontmatter, which a CLI loads every turn
// to decide when to use the skill, and body is the rest, loaded when the
// skill is invoked. A file without frontmatter is all body.
func Estimate(skill []byte) (always, body int) {
	front, rest := splitFrontmatter(skill)
	return tokens(front), tokens(rest)
}

func tokens(b []byte) int { return (len(b) + 3) / 4 }

// splitFrontmatter splits a leading "---" block, both fences included,
// from the rest.
func splitFrontmatter(b []byte) (front, rest []byte) {
	const fence = "---\n"
	if !bytes.HasPrefix(b, []byte(fence)) {
		return nil, b
	}
	end := bytes.Index(b[len(fence):], []byte("\n"+fence))
	if end < 0 {
		return nil, b
	}
	cut := len(fence) + end + 1 + len(fence)
	return b[:cut], b[cut:]
}
