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

// Render picks what vendor v takes. Codex takes nothing yet, TODO(T6955): its
// rule files are global, and the hook that would answer per repo is not built
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
	} else {
		r.renderGrok(mine, s.Trusted)
	}
	if s.Withhold != "" {
		for _, a := range r.Allow {
			r.Skipped = append(r.Skipped, Skip{Rule{Allow, a, "", ""}, s.Withhold})
		}
		r.Allow = nil
	}
	return r
}

// renderGrok is Grok's share. A deny or ask that cannot reach Grok could have
// stopped one of the allows, and working out which is not safe (Bash rules are
// not the only kind), so with one, no allow from this layer goes to Grok at all.
func (r *Rendered) renderGrok(mine []Rule, trusted bool) {
	lost := ""
	for _, rule := range mine {
		switch rule.Kind {
		case Ask:
			// TODO(T6955): Grok's guide and cli.rs at 2bdd1d6a document --allow
			// and --deny only; a flag for ask rules is unverified.
			why := "Grok has no flag for ask rules that aggy can confirm (unverified)"
			r.Skipped = append(r.Skipped, Skip{rule, why})
			lost = rule.Text
		case Deny:
			if why := grokRuleProblem(rule.Text); why != "" {
				r.Skipped = append(r.Skipped, Skip{rule, why})
				lost = rule.Text
				continue
			}
			r.Deny = append(r.Deny, rule.Text)
		}
	}
	for _, rule := range mine {
		if rule.Kind != Allow {
			continue
		}
		why := grokAllowProblem(rule.Text, trusted)
		if why == "" && lost != "" {
			why = "the rule " + lost + " cannot reach Grok, and it might have stopped this one, so no allow from aggy goes to Grok"
		}
		if why != "" {
			r.Skipped = append(r.Skipped, Skip{rule, why})
			continue
		}
		r.Allow = append(r.Allow, rule.Text)
	}
}

// grokRuleProblem says why any rule cannot be one value of a Grok flag. The
// flags split on commas (value_delimiter, src/app/cli.rs at 2bdd1d6a).
func grokRuleProblem(rule string) string {
	tool, _, _, err := ParseRule(rule)
	switch {
	case err != nil:
		return err.Error()
	case !slices.Contains(grokTools, tool):
		return "Grok writes rules for this tool differently, and the mapping is unverified"
	case strings.Contains(rule, ","):
		return "Grok splits a flag's value on commas"
	}
	return ""
}

// grokAllowProblem says why an allow cannot go to Grok as --allow. Grok matches a
// Bash rule as a word-boundary prefix, or as a glob over the whole command, where
// ? and [...] are wildcards too (bash_command_matches_pattern, policy.rs:648 at
// grok-build 2bdd1d6a). So Bash(?? *) would match sh -c evil, and Bash(sudo sh)
// allows sudo sh -c x. Each is judged as what Grok would make of it.
func grokAllowProblem(rule string, trusted bool) string {
	if why := grokRuleProblem(rule); why != "" {
		return why
	}
	if _, _, ok := BashCommand(rule); ok {
		if strings.ContainsAny(rule, "?[") {
			return "? and [ are wildcards on Grok, though not in Claude's rules"
		}
		switch class, why := CheckAllow(PrefixForm(rule)); {
		case class == Drop:
			return "Grok takes an exact rule as a prefix, and that form is too wide: " + why
		case class == RepoCode && !trusted:
			return "Grok takes an exact rule as a prefix, and that form waits for trust: " + why
		}
	}
	return ""
}
