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

// The command skills (T6910): each puts one command inside a session in the
// CLI's menu, /switch, /borrow, and /inbox on Claude and Grok, $switch and so
// on on Codex. aggy catches the command before the model sees it, so a body is
// only what the model reads when that catch missed.
var (
	//go:embed switch/SKILL.md
	Switch []byte
	//go:embed borrow/SKILL.md
	Borrow []byte
	//go:embed inbox/SKILL.md
	Inbox []byte
)

// Skill is one skill aggy links: its folder and invocation name, and its
// SKILL.md.
type Skill struct {
	Name    string
	Content []byte
}

// Own lists aggy's own skills, the aggy skill first.
func Own() []Skill {
	return []Skill{{"aggy", Aggy}, {"switch", Switch}, {"borrow", Borrow}, {"inbox", Inbox}}
}

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

// Front returns the name and description lines of a skill's frontmatter,
// each trimmed of blanks and one pair of quotes, or "" for one it lacks.
// CRLF line ends are read as LF.
// shortcut: single-line values only; a YAML block scalar (| or >) is "".
func Front(skill []byte) (name, description string) {
	front, _ := splitFrontmatter(bytes.ReplaceAll(skill, []byte("\r\n"), []byte("\n")))
	for _, ln := range bytes.Split(front, []byte("\n")) {
		k, v, ok := bytes.Cut(ln, []byte(":"))
		if !ok || len(ln) > 0 && (ln[0] == ' ' || ln[0] == '\t') { // top-level keys only
			continue
		}
		s := string(bytes.TrimSpace(v))
		if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
			s = s[1 : len(s)-1]
		} else if s != "" && (s[0] == '|' || s[0] == '>') {
			s = "" // a block scalar: its text is on the lines below
		}
		switch string(bytes.TrimSpace(k)) {
		case "name":
			name = s
		case "description":
			description = s
		}
	}
	return name, description
}
