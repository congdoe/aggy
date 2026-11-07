package permissions

import (
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// Rendered is what one launch passes to one CLI, on top of what that CLI
// reads itself. Nothing here is written to vendor config.
type Rendered struct {
	Allow, Ask, Deny []string
	// Mode is passed only when it comes from aggy's own settings: a mode
	// set in Claude's files is read by Claude and Grok themselves.
	Mode string
	// Skipped are rules this CLI will not get, and why.
	Skipped []Skip
}

// ReadsItself says the CLI loads rules from that origin on its own. Grok reads
// ~/.claude/settings.json and a project's settings.local.json
// (user-guide/22-permissions-and-safety.md, grok-build 2bdd1d6a), so passing
// them again changes nothing. aggy cannot filter what a CLI reads itself.
func ReadsItself(v adapter.Vendor, o Origin) bool {
	switch v {
	case adapter.Claude:
		return o == FromClaudeUser || o == FromClaudeLocal
	case adapter.Grok:
		return o == FromClaudeUser || o == FromClaudeLocal || o == FromGrokGrant
	}
	return false
}

// grokTools are the tools Grok's guide writes rules for. A Claude MCP rule
// (mcp__x) has another spelling there, MCPTool(x), and what --allow does with
// a name it does not know is unverified, so anything else is named, not sent.
var grokTools = []string{"Bash", "Read", "Edit", "Grep", "WebFetch"}

// Render picks what vendor v takes. Codex takes nothing yet: its rule files
// are global, and the hook that would answer per repo is not built
// (docs/design.md, section 10). OpenCode has its own rendering
// (parity.OpenCodeShared).
func (s Set) Render(v adapter.Vendor) Rendered {
	var r Rendered
	if s.ModeFrom == FromAggy || s.ModeFrom == FromConfig {
		r.Mode = s.Mode
	}
	if v != adapter.Claude && v != adapter.Grok {
		r.Mode = ""
		return r
	}
	var mine []Rule
	for _, rule := range s.Rules {
		if !ReadsItself(v, rule.Origin) {
			mine = append(mine, rule)
		}
	}
	if v == adapter.Claude {
		for _, rule := range mine {
			switch rule.Kind {
			case Allow:
				r.Allow = append(r.Allow, rule.Text)
			case Ask:
				r.Ask = append(r.Ask, rule.Text)
			case Deny:
				r.Deny = append(r.Deny, rule.Text)
			}
		}
		return r
	}
	r.renderGrok(mine)
	return r
}

func (r *Rendered) renderGrok(mine []Rule) {
	var unsentAsks []string
	for _, rule := range mine {
		switch {
		case rule.Kind == Ask:
			// TODO(T6913): Grok's guide documents --allow and --deny only; a
			// flag for ask rules is unverified. A live grok --help would
			// confirm one.
			r.Skipped = append(r.Skipped, Skip{rule, "Grok has no flag for ask rules that aggy can confirm (unverified)"})
			unsentAsks = append(unsentAsks, rule.Text)
		case rule.Kind == Deny && strings.Contains(rule.Text, ","):
			r.Skipped = append(r.Skipped, Skip{rule, "Grok splits a --deny value on commas"})
		case rule.Kind == Deny:
			r.Deny = append(r.Deny, rule.Text)
		}
	}
	for _, rule := range mine {
		if rule.Kind != Allow {
			continue
		}
		if why := grokAllowProblem(rule.Text, unsentAsks); why != "" {
			r.Skipped = append(r.Skipped, Skip{rule, why})
			continue
		}
		r.Allow = append(r.Allow, rule.Text)
	}
}

// grokAllowProblem says why an allow cannot go to Grok as --allow.
func grokAllowProblem(rule string, asks []string) string {
	tool, _, _, err := ParseRule(rule)
	switch {
	case err != nil:
		return err.Error()
	case !slices.Contains(grokTools, tool):
		return "Grok writes rules for this tool differently, and the mapping is unverified"
	case strings.Contains(rule, ","):
		return "Grok splits an --allow value on commas"
	}
	// Grok matches a Bash rule as a prefix with no word boundary
	// (22-permissions-and-safety.md, "Bash Rules"), so one word would also
	// allow every longer command that starts with it.
	if cmd, wild, ok := BashCommand(rule); ok && !wild && !strings.Contains(cmd, " ") {
		return "Grok would take one word as a prefix: " + cmd + " would also match every command starting with it"
	}
	for _, a := range asks {
		if Overlap(rule, a) {
			return "it overlaps " + a + ", an ask rule Grok cannot get, and an ask beats an allow"
		}
	}
	return ""
}

// Overlap reports that two rules could match the same call: the same rule, or
// Bash rules where the fixed words of one start the other.
func Overlap(a, b string) bool {
	if a == b {
		return true
	}
	ca, wa, oka := BashCommand(a)
	cb, wb, okb := BashCommand(b)
	if !oka || !okb {
		return false
	}
	fixed := func(c string, wild bool) string {
		if !wild {
			return c
		}
		f, _, _ := strings.Cut(c, "*")
		return strings.TrimSpace(f)
	}
	fa, fb := fixed(ca, wa), fixed(cb, wb)
	return (wa && strings.HasPrefix(fb, fa)) || (wb && strings.HasPrefix(fa, fb))
}
