package parity

import (
	"errors"
	"io/fs"
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
	// Strict rules (permissions.StrictGroup) are a tool.use deny on OpenCode,
	// and suggest never proposes a command that names one.
	Strict bool
}

// ProtectedBranches are always held to the strict force-push rules, besides
// the default branch read from the repo, which may not be recorded.
var ProtectedBranches = []string{"main", "master", "trunk"}

// branchName is a branch aggy may write into a rule: no glob, comma, space,
// or parenthesis, which would change what the rule matches.
var branchName = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)

// PolicyRules lists every rule, as an on-screen session or a headless one gets
// it. They differ in the .env names: headless, every rule is a deny nobody is
// asked about, so .env.* stays; on screen the usual names are listed
// (envFilesOnScreen), as no glob all the CLIs read alike leaves the templates
// out. The strict force-push group covers ProtectedBranches and branches; a
// name that is not plain is left out.
func PolicyRules(onScreen bool, branches []string) []PolicyRule {
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
	env := envFiles
	if onScreen {
		env = envFilesOnScreen
	}
	add(permissions.PolicyEnv, read(env))
	add(permissions.PolicyKeys, read(privateKeyFiles))
	add(permissions.PolicyPushMain, pushMainRules)
	add(permissions.PolicyForce, forceRules)
	var seen []string
	for _, b := range append(slices.Clone(ProtectedBranches), branches...) {
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
	// OnScreen is a session a person sees and can answer, where a rule can
	// ask. Anything else (a borrow child, one reopened on screen, a headless
	// run) gets every rule as a deny, whatever is set.
	OnScreen bool
	// Settings are the person's group settings (permissions.Set.Policy).
	Settings map[string]permissions.Kind
	// Branches are default branches besides ProtectedBranches.
	Branches []string
}

// PolicyDecision is what one rule becomes for a launch: Deny or Ask.
type PolicyDecision struct {
	PolicyRule
	Kind permissions.Kind
}

// Decide decides every rule. No allow, from any source, changes a decision:
// a rule's rendering is the same whatever allows exist. Leaving a rule out
// for a narrow allow is unsound, as an allow aggy cannot see (a Claude
// settings file or flag it does not read, a mode changed after launch) may
// cover the rest of what the rule matched.
//
// TODO(T6976): exact per-command pre-approval belongs at a per-command
// decision point, a tool-use hook decision, never in the rendered rules.
//
// A headless plan's decisions are ClaudeDeny's rules, in that order: the
// strict force-push rules are left out wherever the force-push group denies,
// as its rules already cover them.
func (p PolicyPlan) Decide() []PolicyDecision {
	forceDenied := !p.OnScreen || p.Settings[permissions.PolicyForce] == permissions.Deny
	var out []PolicyDecision
	for _, r := range PolicyRules(p.OnScreen, p.Branches) {
		d := PolicyDecision{PolicyRule: r, Kind: permissions.Deny}
		if p.OnScreen && p.Settings[r.Group] != permissions.Deny {
			d.Kind = permissions.Ask
		}
		if r.Group == permissions.PolicyForceMain && forceDenied && d.Kind == permissions.Deny {
			continue
		}
		out = append(out, d)
	}
	return out
}

// CoveredRules lists the policy rules (as an on-screen session gets them) an
// allow names or covers. Such an allow changes none of them: aggy permissions
// add says so, Grok is not given the allow, and suggest never proposes a
// command that names a strict one.
func CoveredRules(allow string, branches []string) []PolicyRule {
	var out []PolicyRule
	for _, r := range PolicyRules(true, branches) {
		if Names(r.Text, allow) || covers(allow, r.Text) {
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
// only ever errs toward naming more rules, the safe side for what it gates.
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
// allow"), so a rule asks every time, whatever allows exist.
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
// grok-build 2bdd1d6a): every rule is a deny there, never an allow.
func GrokPolicy(ds []PolicyDecision) (deny []string) {
	for _, d := range ds {
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
// (verification.md, "OpenCode 2.2.5"), every ask is a deny. The strict rules stay denies: OpenCode may keep an Always
// answer as a wildcard that beats a later ask (design.md section 15), so one
// answer could silence them for good; TODO(T6895), a live check. File reads
// are not here: the guard refuses those names whatever is set.
func OpenCodePolicy(ds []PolicyDecision, auto bool) (ask, deny []string) {
	for _, d := range ds {
		cmd, ok := strings.CutPrefix(d.Text, "Bash(")
		if !ok {
			continue
		}
		cmd = strings.TrimSuffix(cmd, ")")
		if d.Kind == permissions.Deny || d.Strict || auto {
			deny = append(deny, "shell:"+cmd)
		} else {
			ask = append(ask, cmd)
		}
	}
	return ask, deny
}

// DefaultBranch reads the repo's default branch from
// refs/remotes/origin/HEAD in its common git dir, as git clone and git remote
// set-head write it ("ref: refs/remotes/origin/<branch>"), without running
// git. known is false when it could not be read: a reftable repo keeps it
// elsewhere, and a packed-refs file holds no symbolic ref. It is "" with known
// true when the repo records none.
func DefaultBranch(root string) (name string, known bool) {
	common, err := GitCommonDir(root)
	if err != nil {
		return "", false
	}
	if _, err := safefs.Lstat(filepath.Join(common, "reftable")); err == nil {
		return "", false
	}
	b, err := safefs.ReadFileLimit(filepath.Join(common, "refs", "remotes", "origin", "HEAD"), maxGitFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", true
	case err != nil:
		return "", false
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/remotes/origin/")
	if !ok || !branchName.MatchString(name) || strings.Contains(name, "..") {
		return "", false
	}
	return name, true
}
