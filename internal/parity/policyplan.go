package parity

import (
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/safefs"
)

// PolicyRule is one rule of the safety policy, in Claude's spelling.
type PolicyRule struct {
	Group string // a permissions.Policy* group
	Text  string
	// Strict rules are never pre-approved by an answer at a CLI's prompt, nor
	// by an allow that only happens to cover them (permissions.StrictGroup).
	Strict bool
}

// branchName is a branch aggy may write into a rule: no glob, comma, space,
// or parenthesis, which would change what the rule matches.
var branchName = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)

// PolicyRules lists every rule, with a strict force-push group for main and
// for each extra branch (the repo's default branch, when it is not main). A
// branch name that is not plain is left out.
func PolicyRules(branches []string) []PolicyRule {
	var out []PolicyRule
	add := func(group string, rules []string) {
		for _, r := range rules {
			out = append(out, PolicyRule{Group: group, Text: r, Strict: permissions.StrictGroup(group)})
		}
	}
	read := func(names []string) []string {
		var rs []string
		for _, n := range names {
			rs = append(rs, "Read(//**/"+n+")")
		}
		return rs
	}
	add(permissions.PolicyEnv, read(envFiles))
	add(permissions.PolicyKeys, read(privateKeyFiles))
	add(permissions.PolicyPushMain, pushMainRules)
	add(permissions.PolicyForce, forceRules)
	seen := []string{}
	for _, b := range append([]string{"main"}, branches...) {
		if slices.Contains(seen, b) || !branchName.MatchString(b) || strings.Contains(b, "..") {
			continue
		}
		seen = append(seen, b)
		add(permissions.PolicyForceMain, forceBranchRules(b))
	}
	add(permissions.PolicyRmRf, rmRules)
	add(permissions.PolicyReset, resetRules)
	add(permissions.PolicyClean, cleanRules)
	return out
}

// PolicyPlan says how a launch gets the safety policy.
type PolicyPlan struct {
	// OnScreen is a session the person sees, where a rule can ask. A borrow
	// child, even one reopened on screen, gets every rule as a deny.
	OnScreen bool
	// Settings are the person's group settings (permissions.Set.Policy).
	Settings map[string]permissions.Kind
	// Allows may pre-approve a rule (permissions.Set.PolicyAllows).
	Allows []permissions.Rule
	// Branches are default branches besides main, for the strict group.
	Branches []string
}

// PolicyDecision is what one rule becomes for a launch.
type PolicyDecision struct {
	PolicyRule
	// Kind is Deny or Ask, or Allow when an allow pre-approved it, so it is
	// not rendered at all: the CLI then does what it does without aggy's rule.
	Kind permissions.Kind
	// By is the allow that pre-approved it.
	By permissions.Rule
}

// Decide decides every rule. A deny the person set always holds, whatever
// allows say; an ask can be pre-approved.
//
// A borrow child's decisions are ClaudeDeny's rules, in that order: the strict
// force-push rules are left out wherever the force-push group denies, as its
// rules already cover them.
func (p PolicyPlan) Decide() []PolicyDecision {
	forceDenied := !p.OnScreen || p.Settings[permissions.PolicyForce] == permissions.Deny
	var out []PolicyDecision
	for _, r := range PolicyRules(p.Branches) {
		d := PolicyDecision{PolicyRule: r, Kind: permissions.Deny}
		if p.OnScreen && p.Settings[r.Group] != permissions.Deny {
			d.Kind = permissions.Ask
			if a, ok := preApproval(r, p.Allows); ok {
				d.Kind, d.By = permissions.Allow, a
			}
		}
		if r.Group == permissions.PolicyForceMain && forceDenied && d.Kind == permissions.Deny {
			continue
		}
		out = append(out, d)
	}
	return out
}

// preApproval finds an allow that pre-approves r. An ordinary rule is
// pre-approved by an allow that names it (matches what the rule matches,
// such as Bash(rm -rf dist) for Bash(rm -rf*)) or covers it (Bash(rm *)). A
// strict rule only by one that names it, and only from aggy's own file: the
// person wrote that deliberately, and an answer at a CLI's prompt, which no CLI
// lets aggy keep from offering, does not count.
func preApproval(r PolicyRule, allows []permissions.Rule) (permissions.Rule, bool) {
	for _, a := range allows {
		if a.Kind != permissions.Allow {
			continue
		}
		if r.Strict {
			if (a.Origin == permissions.FromAggy || a.Origin == permissions.FromAggyRepo) && Names(r.Text, a.Text) {
				return a, true
			}
			continue
		}
		if Names(r.Text, a.Text) || covers(a.Text, r.Text) {
			return a, true
		}
	}
	return permissions.Rule{}, false
}

// PreApproves lists the rules an allow in aggy's own file would pre-approve,
// for aggy permissions add to name, and suggest to leave out.
func PreApproves(allow string, branches []string) []PolicyRule {
	a := permissions.Rule{Kind: permissions.Allow, Text: allow, Origin: permissions.FromAggy}
	var out []PolicyRule
	for _, r := range PolicyRules(branches) {
		if _, ok := preApproval(r, []permissions.Rule{a}); ok {
			out = append(out, r)
		}
	}
	return out
}

// ruleParts splits a Bash or Read rule into its tool and what it matches: a
// Bash rule's command, or a Read rule's path with its anchor (//, /, ~/, ./)
// taken off. ok is false for anything else.
func ruleParts(rule string) (tool, spec string, has, ok bool) {
	tool, spec, has, err := permissions.ParseRule(rule)
	if err != nil || (tool != "Bash" && tool != "Read") {
		return "", "", false, false
	}
	if tool == "Bash" && has {
		spec, _, _ = permissions.BashCommand(rule)
	}
	if tool == "Read" {
		for _, p := range []string{"//", "~/", "./", "/"} {
			if s, cut := strings.CutPrefix(spec, p); cut {
				spec = s
				break
			}
		}
	}
	return tool, spec, has, true
}

// Names says the allow names policy rule r: what the allow matches is within
// what r matches, its own * read as text. A Read allow names a Read rule
// when its file name matches the rule's (Read(./certs/server.pem) names
// Read(//**/*.pem)).
func Names(r, allow string) bool {
	rt, rs, _, ok := ruleParts(r)
	at, as, ahas, aok := ruleParts(allow)
	if !ok || !aok || rt != at || !ahas {
		return false
	}
	if rt == "Read" {
		return glob(path.Base(rs), path.Base(as))
	}
	return glob(rs, as)
}

// covers says the allow matches everything policy rule r matches, r's own *
// read as text: Bash(git push *) covers Bash(git push * main). A bare Bash or
// Read covers every rule of that tool.
func covers(allow, r string) bool {
	rt, rs, _, ok := ruleParts(r)
	at, as, ahas, aok := ruleParts(allow)
	if !ok || !aok || rt != at {
		return false
	}
	if !ahas {
		return true
	}
	if rt == "Read" {
		// The rule matches the name at any depth: the allow must too.
		return glob(as, "d/"+path.Base(rs)) && glob(strings.TrimPrefix(as, "**/"), path.Base(rs))
	}
	return glob(as, rs)
}

// glob matches s against pattern, where * matches any text, / included. It
// over-reads ** and a Read rule's single * alike, which for Names and covers
// only ever errs toward fewer pre-approvals of a deeper path.
func glob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// ClaudePolicy renders the decisions for Claude: asks in permissions.ask and
// denies in permissions.deny of the same inline --settings layer
// (permissions.md: "Rules are evaluated in order: deny, then ask, then
// allow"). A pre-approved rule is left out.
func ClaudePolicy(ds []PolicyDecision) (ask, deny []string) {
	for _, d := range ds {
		switch d.Kind {
		case permissions.Ask:
			ask = append(ask, d.Text)
		case permissions.Deny:
			deny = append(deny, d.Text)
		}
	}
	return ask, deny
}

// GrokPolicy renders them for Grok, which takes --deny and --allow on its
// command line and no flag for ask rules that aggy can confirm (cli.rs at
// grok-build 2bdd1d6a): an ask stays a deny there, never an allow. A
// pre-approved rule is left out, so Grok does what it does without it.
func GrokPolicy(ds []PolicyDecision) (deny []string) {
	for _, d := range ds {
		if d.Kind == permissions.Allow {
			continue
		}
		deny = append(deny, grokSpelling(d.Text))
	}
	return deny
}

// grokSpelling is a rule in Grok's syntax: no // anchor, an unrooted **/
// matches anywhere.
func grokSpelling(r string) string {
	if rest, ok := strings.CutPrefix(r, "Read(//**/"); ok {
		return "Read(**/" + rest
	}
	return r
}

// OpenCodePolicy renders the shell rules for OpenCode: an ask goes last in its
// permissions, where a later rule wins over the person's allows, and a deny is
// a tool.use policy. Under --auto, which answers an ask with once
// (verification.md, "OpenCode 2.2.5"), every ask is a deny. File reads are
// not here: the guard refuses those names whatever is set (OpenCodeDeniedNames).
func OpenCodePolicy(ds []PolicyDecision, auto bool) (ask, deny []string) {
	for _, d := range ds {
		cmd, ok := strings.CutPrefix(d.Text, "Bash(")
		if !ok {
			continue
		}
		cmd = strings.TrimSuffix(cmd, ")")
		switch {
		case d.Kind == permissions.Deny, d.Kind == permissions.Ask && auto:
			deny = append(deny, "shell:"+cmd)
		case d.Kind == permissions.Ask:
			ask = append(ask, cmd)
		}
	}
	return ask, deny
}

// DefaultBranch reads the repo's default branch from
// refs/remotes/origin/HEAD in its common git dir, as git clone and git remote
// set-head write it ("ref: refs/remotes/origin/<branch>"), without running
// git. It is "" when that file is not there or names no plain branch.
func DefaultBranch(root string) string {
	common, err := GitCommonDir(root)
	if err != nil {
		return ""
	}
	b, err := safefs.ReadFileLimit(filepath.Join(common, "refs", "remotes", "origin", "HEAD"), maxGitFile)
	if err != nil {
		return ""
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/remotes/origin/")
	if !ok || !branchName.MatchString(name) || strings.Contains(name, "..") {
		return ""
	}
	return name
}
