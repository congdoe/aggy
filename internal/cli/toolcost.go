package cli

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/tooldef"
	"github.com/congdoe/aggy/skills"
)

// toolCostNote explains the cost aggy tool list and aggy doctor show.
const toolCostNote = "Cost per vendor, here: estimated tokens every turn, +N more when a skill is invoked (a skill's SKILL.md bytes / 4, or the definition's [cost]); 0 when off or not wired; unknown when aggy cannot tell without starting the server."

// toolCost estimates, offline, the tokens tool t costs vendor v: "0"
// when it is off for v (on is its switchboard state) or nothing of it
// is wired there (Grok gets no MCP server or hook while its wiring is
// gated). Otherwise by the definition's [cost] method: static is its
// tokens for v, or for every vendor; skill-md is the installed skill's
// SKILL.md as aggy skill status counts its own (skills.Estimate: bytes
// / 4, the frontmatter every turn and the body on invoke), with
// "+unknown" when an MCP server or hook is wired too; anything else, an
// MCP server's schemas included, is "unknown", since aggy never starts
// a server to count them.
func toolCost(t tooldef.Tool, v adapter.Vendor, on bool) string {
	p := t.PartsFor(v)
	if v == adapter.Grok && !grokToolsVerified {
		p.MCP, p.Hooks = nil, nil
	}
	if !on || (p.MCP == nil && p.Skill == nil && len(p.Hooks) == 0) {
		return "0"
	}
	switch t.Cost.Method {
	case "static":
		n, ok := t.Cost.Vendor[string(v)]
		if !ok {
			n = t.Cost.Tokens
		}
		if n > 0 {
			return "~" + strconv.Itoa(n)
		}
	case "skill-md":
		if p.Skill == nil {
			break
		}
		s := skillCost(t)
		if s != "unknown" && (p.MCP != nil || len(p.Hooks) > 0) {
			s += " +unknown"
		}
		return s
	}
	return "unknown"
}

// skillCost is the estimate of t's installed skill (toolCost): "~N +M",
// N every turn and M more on invoke; "unknown" when its folder is not
// in place or is refused.
func skillCost(t tooldef.Tool) string {
	l, err := toolSkillLayout(t)
	if err == nil {
		err = checkSkillStore(l.store)
	}
	if err != nil {
		return "unknown"
	}
	b, err := safefs.ReadFileLimit(filepath.Join(l.store, "SKILL.md"), maxSkillBytes)
	if err != nil {
		return "unknown"
	}
	always, body := skills.Estimate(b)
	return fmt.Sprintf("~%d +%d", always, body)
}
