package parity

import (
	"strings"

	"github.com/congdoe/aggy/internal/permissions"
)

// HermesDeny renders the policy's denies for Hermes' approvals.deny: the
// shell rules of every group the person set to deny, as fnmatch globs.
// Hermes matches each against a terminal command's whole text and its
// de-obfuscated variants, lower case (tools/approval_floors.py:23-42 at
// v0.23.1), so a glob starts with * to match the rule's command anywhere in
// the line, as Claude's rule matches it in any subcommand: it may match more
// than the rule does, never less, the safe side for a deny. fnmatch's [ and ?
// are taken literally.
//
// Asks are not here: an approvals.deny entry refuses outright, even under
// --yolo. aggy's pre_tool_call hook asks for them (policyhook.JudgeHermes),
// and refuses the denies again for every tool it reads. Read rules are not
// here either: approvals.deny covers terminal commands only, and Hermes' own
// guard says file-read denies are "not a security boundary; the terminal tool
// can still bypass" (agent/file_safety.py:343-345); the hook judges the file
// tools.
func HermesDeny(ds []PolicyDecision) []string {
	var out []string
	for _, d := range ds {
		cmd, ok := strings.CutPrefix(d.Text, "Bash(")
		if !ok || d.Kind != permissions.Deny {
			continue
		}
		cmd = strings.TrimSuffix(cmd, ")")
		// Claude's legacy prefix form, rm:*, is the prefix and anything after.
		if p, ok := strings.CutSuffix(cmd, ":*"); ok {
			cmd = p + "*"
		}
		g := "*" + strings.NewReplacer("[", "[[]", "?", "[?]").Replace(strings.ToLower(cmd))
		if !strings.HasSuffix(g, "*") {
			g += "*"
		}
		out = append(out, g)
	}
	return out
}
