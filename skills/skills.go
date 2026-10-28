// Package skills embeds the skills aggy links into the vendor CLIs, so
// the binary carries them and aggy skill link can install them without
// a source checkout.
package skills

import _ "embed"

// Aggy is skills/aggy/SKILL.md, the aggy skill.
//
//go:embed aggy/SKILL.md
var Aggy []byte
