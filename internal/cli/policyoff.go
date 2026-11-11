package cli

import (
	"fmt"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/permissions"
)

// policyOffLine is the one line said while any group is off (allow, T6979):
// at each on-screen launch and in aggy permissions show. "" when none is.
func policyOffLine(s permissions.Set) string {
	off := s.OffGroups()
	if len(off) == 0 {
		return ""
	}
	return fmt.Sprintf("the safety policy is off on screen for %s, as you set (aggy permissions policy GROUP ask turns one back on); Codex still asks, and borrows still refuse", strings.Join(off, ", "))
}

// repoPolicyLine says a repo's [policy] table is ignored: the safety
// policy's settings are only ever the person's own (T6979).
func repoPolicyLine(rc config.RepoConfig) string {
	if !rc.SetsPolicy() {
		return ""
	}
	return "this repo's .aggy/config.toml has a [policy] table, which aggy ignores: only your own ~/.aggy/permissions.json sets the safety policy (aggy permissions policy)"
}

// policyVendorLine is the one launch line, on screen, for a CLI where the
// safety policy cannot ask the way it does on Claude Code and OpenCode
// (T6979): what it does there, and the ways forward. "" for any other CLI,
// or on Grok when every group is off.
//
// Grok takes --deny and no ask rule aggy can confirm (cli.rs at grok-build
// 2bdd1d6a; parity.GrokPolicy), so a match is refused with no prompt. Codex
// takes the strictest decision of every rule that matches, so an allow it
// saves cannot loosen aggy's prompt rule (execpolicy/README.md and
// core/src/exec_policy.rs at rust-v0.164.0; parity.CodexRules), and no hook
// carries aggy's pre-approvals there. Codex's rules file is global and a
// borrow child relies on it, so a group turned off does not reach it.
func policyVendorLine(v adapter.Vendor, s permissions.Set) string {
	switch v {
	case adapter.Grok:
		if len(s.OffGroups()) == len(permissions.PolicyGroups) {
			return ""
		}
		return "on Grok Build, a command or file aggy's safety policy matches is refused, with no prompt: Grok takes no ask rule from aggy. To do that step, do it on Claude Code or OpenCode, or turn its group off with aggy permissions policy GROUP allow"
	case adapter.Hermes:
		// T6933: approvals.deny and Hermes' approval prompt cover terminal
		// commands only (tools/approval_floors.py at v0.23.1); aggy's hook
		// judges the file tools too.
		return "on Hermes Agent, aggy's safety policy refuses what it denies and asks the rest through Hermes' own approval prompt; Hermes' approvals cover terminal commands only, and its terminal tool can read any file you can, so aggy's hook catches mistakes on its file tools, not a determined model. aggy permissions policy GROUP allow turns a group off"
	case adapter.Codex:
		return "on Codex, aggy's safety policy asks before each match, every time, and an exact pre-approval (aggy permissions add allow) does not apply. To skip the ask, do that step on Claude Code or OpenCode; aggy permissions policy GROUP allow turns a group off there, not on Codex, whose rules file is global and keeps borrows refused"
	}
	return ""
}
