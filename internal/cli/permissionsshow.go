package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/termsafe"
)

// permissionsShow is aggy permissions show: the rules in force, where each came
// from, what each CLI gets, and what was skipped and why. It only reads.
// TODO(T6908): /aggy permissions in a session shows this too, once batch 6's
// /aggy commands exist.
func permissionsShow(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return cmdUsage(stderr, "permissions show", "takes no arguments", permissionsUsage)
	}
	s, in, cfg, err := permissionsHere(context.Background())
	if err != nil {
		return cmdErr(stderr, "permissions show", diag.CodeConfig, err)
	}
	printPermissionsHeader(stdout, s, in)
	if cfg.Policy.Required {
		fmt.Fprintf(stdout, "Safety policy: on, and its rules stay on top of everything below (aggy policy --help).\n")
	} else {
		fmt.Fprintf(stdout, "Safety policy: on where a CLI can take it; [policy] required = false lets a CLI start without it.\n")
	}
	printRules(stdout, s)
	printPolicy(stdout, s)

	fmt.Fprintln(stdout, "\nWhat each CLI gets, at its next launch or switch")
	claude := s.Render(adapter.Claude)
	fmt.Fprintf(stdout, "  Claude Code: reads its own settings files, and takes %s from aggy\n", ruleCounts(claude.Allow, claude.Ask, claude.Deny))
	grok := s.Render(adapter.Grok)
	fmt.Fprintf(stdout, "  Grok Build: reads Claude's settings files itself, unfiltered, and takes %s from aggy\n", ruleCounts(grok.Allow, nil, grok.Deny))
	shared, openCodeSkipped := parity.OpenCodeShared(s)
	fmt.Fprintf(stdout, "  OpenCode: takes %s, exact commands only, besides its shipped list\n", ruleCounts(shared.Allow, shared.Ask, shared.Deny))
	fmt.Fprintln(stdout, "  Codex: takes none of these yet. It keeps asking in its own way, and its own")
	fmt.Fprintln(stdout, "    approvals still count; the hook that would answer for it is not built")

	printed := false
	skip := func(k permissions.Skip, on string) {
		if !printed {
			fmt.Fprintln(stdout, "\nSkipped")
			printed = true
		}
		fmt.Fprintf(stdout, "  %s %s  [%s]%s\n    %s\n", k.Kind, termsafe.Field(k.Text), originName(k.Origin), on, termsafe.Line(k.Why))
		if self := appliedBy(k.Origin); self != "" && on == "" {
			fmt.Fprintf(stdout, "    aggy does not pass this on, but%s, from its own file\n", self)
		}
	}
	for _, k := range s.Skipped {
		skip(k, "")
	}
	for _, k := range grok.Skipped {
		skip(k, " on Grok")
	}
	for _, k := range openCodeSkipped {
		skip(k, " on OpenCode")
	}
	printPermissionsTroubles(stdout, s)
	fmt.Fprintln(stdout, "\nNot shown: OpenCode's Always allow, which it keeps in opencode.db beside its sign-ins, and aggy never opens that.")
	return 0
}

// policyGroupNames are the safety policy's groups in a person's words.
var policyGroupNames = map[string]string{
	permissions.PolicyEnv:       "reading .env files (not .env.example, .env.sample, .env.template, .env.dist)",
	permissions.PolicyKeys:      "reading private key files (*.pem, *.key, id_rsa and the like), strict",
	permissions.PolicyPushMain:  "a push to main",
	permissions.PolicyForce:     "a force push",
	permissions.PolicyForceMain: "a force push to main or the default branch, strict",
	permissions.PolicyRmRf:      "a recursive forced delete (rm -rf)",
	permissions.PolicyReset:     "git reset --hard",
	permissions.PolicyClean:     "git clean",
}

// printPolicy says how the safety policy reaches each CLI in this folder: each
// group's setting, the allows that name or cover a rule (they change none),
// and what a CLI cannot express.
func printPolicy(w io.Writer, s permissions.Set) {
	cwd, _ := os.Getwd()
	plan := policyPlan(planOnScreen, cwd, s)
	ds := plan.Decide()
	fmt.Fprintln(w, "\nSafety policy, on screen (a borrow child and a headless run are always refused)")
	for _, g := range permissions.PolicyGroups {
		setting, from := "ask", "the default"
		if k, ok := s.Policy[g]; ok {
			setting, from = string(k), originName(s.PolicyFrom[g])
		}
		fmt.Fprintf(w, "  %s: %s [%s]  %s\n", g, setting, from, policyGroupNames[g])
		rules := slices.Clone(s.Rules)
		for _, k := range s.Skipped {
			rules = append(rules, k.Rule)
		}
		for i, r := range rules {
			if r.Kind != permissions.Allow {
				continue
			}
			m := policyMatch(r.Text, cwd)
			if !slices.Contains(m.groups, g) {
				continue
			}
			what := "names or covers this group and pre-approves nothing: what it covers still asks on Claude Code and OpenCode"
			switch {
			case setting == string(permissions.Deny):
				what = "matches this group, which is set to deny, so it is refused"
			case setting == string(permissions.Allow):
				what = "matches this group, which is off here, so aggy adds nothing to it but on Codex, which still asks"
			case m.exact && (r.Origin == permissions.FromAggy || r.Origin == permissions.FromAggyRepo) && i >= len(s.Rules):
				what = "would pre-approve this exact command, but it does not apply here (aggy permissions show says why), so the command still asks"
			case m.exact && (r.Origin == permissions.FromAggy || r.Origin == permissions.FromAggyRepo) && isRead(r.Text) && !s.Trusted:
				what = "would pre-approve this exact file, but a read pre-approval applies only in a repo you trusted (aggy trust), so the read still asks"
			case m.exact && (r.Origin == permissions.FromAggy || r.Origin == permissions.FromAggyRepo) && s.Withhold != "":
				what = "would pre-approve this exact command, but aggy sends no allow until the problem below is fixed, so the command still asks"
			case m.exact && (r.Origin == permissions.FromAggy || r.Origin == permissions.FromAggyRepo):
				what = "pre-approves this exact command or file: it runs without asking on Claude Code and OpenCode"
			case m.exact:
				what = "is not in aggy's own file, so it pre-approves nothing: the command still asks"
			}
			fmt.Fprintf(w, "    your allow %s [%s] %s\n", termsafe.Field(r.Text), originName(r.Origin), what)
		}
	}
	if _, known := defaultBranches(cwd); !known {
		fmt.Fprintln(w, "  aggy could not read this repo's default branch, so only main, master, and trunk are strict")
	}
	if l := policyOffLine(s); l != "" {
		fmt.Fprintf(w, "  %s\n", l)
	}
	ask, deny := parity.ClaudePolicy(ds)
	fmt.Fprintf(w, "  Claude Code: %d deny rules, and %d asks made by aggy's decision hook, which asks for every match but an exact pre-approval (static ask rules, which nothing lifts, when hooks are off)\n", len(deny), len(ask))
	fmt.Fprintf(w, "  Grok Build: %d deny rules; Grok has no flag for an ask rule aggy can confirm, so these stay denials there, and an allow that names one is not passed to Grok\n", len(parity.GrokPolicy(ds)))
	oAsk, oDeny := parity.OpenCodePolicy(ds, false)
	fmt.Fprintf(w, "  OpenCode: %d shell asks and %d shell denies (the strict rules ask instead in a repo trusted for OpenCode; all deny under --auto, which answers an ask); aggy's plugin lifts an ask only for an exact pre-approval; private key reads ask there too, but aggy's guard refuses one by another name or through a link, and other secret files stay refused\n", len(oAsk), len(oDeny))
	fmt.Fprintln(w, "  Codex: its rules file asks before each rule, in every repo; the file is global, so a")
	fmt.Fprintln(w, "    setting here does not reach it, it has no rule for file reads, and no pre-approval changes it")
	fmt.Fprintln(w, "  aggy permissions add allow \"Bash(<exact command>)\" pre-approves one command on Claude")
	fmt.Fprintln(w, "    Code and OpenCode; aggy permissions policy <group> deny refuses a group instead")
}

// isRead reports a Read rule.
func isRead(rule string) bool {
	tool, _, _, err := permissions.ParseRule(rule)
	return err == nil && tool == "Read"
}

// ruleCounts says how many rules of each kind.
func ruleCounts(allow, ask, deny []string) string {
	return fmt.Sprintf("%d allow, %d ask, and %d deny", len(allow), len(ask), len(deny))
}
