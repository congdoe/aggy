package cli

import (
	"context"
	"fmt"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/suggest"
)

// A quiet launch (T7015, docs/design.md section 5): aggy prints between the
// person and their CLI only what means a protection is off or missing this
// run, its questions, and its refusals. The status a launch used to print on
// every start, and on every switch, is here instead, for aggy doctor, built
// for the folder doctor runs in from the same functions the launch uses:
// how the safety policy works on each CLI, rules that do not reach a CLI,
// exact pre-approvals that run unasked, a repo's [policy] and [keys] tables
// that aggy ignores, aggy's command skills missing from a CLI's menu, an
// untrusted repo's OpenCode, and commands aggy permissions suggest could
// allow. Each message holds no path, rule text, or command: a path is the
// finding's Path, and anything else its Detail, which a report leaves out.

// Doctor's finding codes for what a launch no longer says.
const (
	codePolicyOnVendor = "policy-on-vendor"    // info: how the safety policy works on this CLI
	codePreApprovals   = "pre-approvals"       // info: exact pre-approvals of policy matches here
	codeRepoIgnored    = "repo-config-ignored" // info: a repo table aggy ignores
	codeCommandSkills  = "command-skills"      // info: aggy's commands not in a CLI's menu
	codeOpenCodeMini   = "opencode-untrusted"  // info: this repo gets opencode mini
	codeSuggest        = "suggest-candidates"  // info: commands aggy permissions suggest could allow
	codeClaudeHooksOff = "claude-hooks-off"    // warning: aggy's guards and pre-approvals are off on Claude Code here
)

// launchStatus is the section's findings for cwd, for each installed CLI of
// vendors. It changes nothing: the index is read read-only, and git only
// asked. "" cwd, or a config or permissions file aggy cannot read (which
// other sections report), gives what can still be said.
func launchStatus(ctx context.Context, cwd string, vendors []checkup.Vendor) []checkup.Finding {
	var out []checkup.Finding
	add := func(code string, v adapter.Vendor, path, msg, detail string) {
		out = append(out, checkup.Finding{Code: code, Level: checkup.LevelInfo, Vendor: v, Path: path, Message: msg, Detail: detail})
	}
	for _, vr := range vendors {
		if !vr.Installed {
			continue
		}
		if ls, names := missingSkills(ctx, vr.Vendor); len(ls) > 0 {
			add(codeCommandSkills, vr.Vendor, "", skillsMissingLine(vr.Vendor, names), "")
		}
	}
	if cwd == "" {
		return out
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return out
	}
	root, _ := repoRoot(cwd)
	if root != "" {
		if rc, err := config.LoadRepo(root); err == nil {
			if l := repoPolicyLine(rc); l != "" {
				add(codeRepoIgnored, "", "", l, "")
			}
			if l := repoKeysLine(rc); l != "" {
				add(codeRepoIgnored, "", "", l, "")
			}
		}
	}
	rules := loadPermissions(ctx, cwd, cfg)
	if rules.FileErr != nil {
		return out
	}
	plan := policyPlan(planOnScreen, cwd, rules)
	for _, vr := range vendors {
		if !vr.Installed {
			continue
		}
		v := vr.Vendor
		if l := policyVendorLine(v, rules); l != "" {
			add(codePolicyOnVendor, v, "", l, "")
		}
		switch v {
		case adapter.Claude, adapter.Grok:
			out = append(out, sharedRules(v, vr.Capabilities, rules, &adapter.InteractiveRequest{}).status...)
		case adapter.OpenCode:
			if _, skipped := parity.OpenCodeShared(rules); skippedAllows(skipped) > 0 {
				add(codePermRules, v, "", skippedNote(skippedAllows(skipped), "OpenCode", !rules.Trusted), "")
			}
			if root != "" {
				if trusted, _ := openCodeTrusted(root); !trusted {
					add(codeOpenCodeMini, v, root, openCodeMiniStatus(rules.HeldLocal), "")
				}
			}
		}
		if v == adapter.Claude {
			s := indexRO(ctx)
			n := preApprovalCount(parity.PreApprovedFrom(rules, root, readIdentities(ctx, s)), plan.Branches)
			if s != nil {
				_ = s.Close()
			}
			off := rules.HooksOff
			if off == "" {
				off = repoHooksOff(cwd, root)
			}
			if f := managedSettingsPath(); off == "" && f != "" && settingsHooksOff(f) {
				off = f
			}
			// #148 review H1: hooks off is a protection missing, said
			// whether or not pre-approvals exist (usePolicyHook's reasons,
			// those a launch knows only from its flags aside).
			hooksOff := "Claude's hooks are off here, so " + guardsOff + " in Claude Code sessions here, and exact pre-approvals do not apply"
			switch {
			case off != "":
				out = append(out, checkup.Finding{Code: codeClaudeHooksOff, Level: checkup.LevelWarn, Vendor: v, Path: off,
					Message: "this Claude settings file turns Claude's hooks off: " + hooksOff})
			case simpleMode(nil):
				out = append(out, checkup.Finding{Code: codeClaudeHooksOff, Level: checkup.LevelWarn, Vendor: v,
					Message: claudeSimpleVar + " is set, which may start Claude without hooks: " + hooksOff})
			default:
				if l := preApprovalNote(n); l != "" {
					add(codePreApprovals, v, "", l, "")
				}
			}
		}
	}
	if repo, err := toolRepo(cwd, false); err == nil && repo.key != "" {
		if s := indexRO(ctx); s != nil {
			c, err := s.SuggestCandidates(ctx, repo.key, suggest.MinCount)
			_ = s.Close()
			switch n := len(c); {
			case err != nil || n == 0:
			case n == 1:
				add(codeSuggest, "", "", "1 command you run often could be allowed here: aggy permissions suggest", "")
			default:
				add(codeSuggest, "", "", fmt.Sprintf("%d commands you run often could be allowed here: aggy permissions suggest", n), "")
			}
		}
	}
	return out
}

// openCodeMiniStatus is doctor's openCodeUntrustedNote: the repo is the
// finding's Path, never its message.
func openCodeMiniStatus(held int) string {
	s := "this repo is not trusted for OpenCode, so an aggy launch of OpenCode here gets opencode mini, the whitelist, and a model chosen at launch; aggy trust --opencode <repo> trusts it"
	switch held {
	case 0:
		return s
	case 1:
		return s + ". Its .claude/settings.local.json has 1 allow rule aggy holds until then"
	}
	return fmt.Sprintf("%s. Its .claude/settings.local.json has %d allow rules aggy holds until then", s, held)
}
