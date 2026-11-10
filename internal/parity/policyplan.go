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
	// Strict rules are never left out of any rendering (permissions.StrictGroup).
	Strict bool
}

// Read says the rule guards a file read. A pre-approval never removes one: a
// CLI reads a file it is not told about with no prompt of its own (Claude in
// its working directory, Grok in every mode, verification.md T6740), so one
// allowed file would expose every other.
func (r PolicyRule) Read() bool { return strings.HasPrefix(r.Text, "Read(") }

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
	// NoPrompt says the session's Claude permission mode does not prompt for
	// a command no rule names (acceptEdits, auto, dontAsk, bypassPermissions),
	// so leaving a rule out would not leave Claude's own prompt behind it:
	// then no pre-approval takes effect.
	NoPrompt bool
	// Settings are the person's group settings (permissions.Set.Policy).
	Settings map[string]permissions.Kind
	// Allows may pre-approve a rule (permissions.Set.PolicyAllows).
	Allows []permissions.Rule
	// Branches are default branches besides ProtectedBranches.
	Branches []string
}

// PolicyDecision is what one rule becomes for a launch.
type PolicyDecision struct {
	PolicyRule
	// Kind is Deny or Ask, or Allow when an allow pre-approved it on Claude:
	// Claude's rendering leaves it out, so Claude's own prompt stands for what
	// it covered. Every other CLI renders an Allow as it would an Ask.
	Kind permissions.Kind
	// By is the allow that pre-approved it.
	By permissions.Rule
	// Unused is an allow in aggy's own file that names or covers the rule but
	// does not pre-approve it, and Why says why: a strict or read rule, or a
	// mode with no prompt behind it.
	Unused permissions.Rule
	Why    string
}

// Decide decides every rule. A deny the person set always holds, whatever
// allows say. A pre-approval removes a rule only where the CLI itself still
// prompts for every other command the rule covered, which today is an
// ordinary Bash rule on Claude in a prompting mode.
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
			p.preApprove(&d)
		}
		if r.Group == permissions.PolicyForceMain && forceDenied && d.Kind == permissions.Deny {
			continue
		}
		out = append(out, d)
	}
	return out
}

func (p PolicyPlan) preApprove(d *PolicyDecision) {
	for _, a := range p.Allows {
		if a.Kind != permissions.Allow || !Names(d.Text, a.Text) && !covers(a.Text, d.Text) {
			continue
		}
		mine := a.Origin == permissions.FromAggy || a.Origin == permissions.FromAggyRepo
		why := ""
		switch {
		case d.Strict:
			// TODO(T6976): an exact per-command pre-approval of a strict rule
			// (the owner's choice for an allow in aggy's file that names it)
			// needs a per-command decision point, a PreToolUse hook decision,
			// as removing the rule would also let every other command it
			// covers through. Until then a strict rule always asks.
			why = "a strict rule; a pre-approval does not change it on any CLI yet"
		case d.Read():
			// TODO(T6976): the same, for one file read.
			why = "a file read rule; no CLI prompts for the other files it covers, so a pre-approval does not change it"
		case p.NoPrompt:
			why = "Claude's permission mode here does not prompt for a command no rule names"
		case !mine && tooWide(a.Text):
			// A CLI's own approval file can be written by another harness's
			// session (settings.local.json), so from there only a rule narrower
			// than one command word and * removes anything.
			continue
		default:
			d.Kind, d.By = permissions.Allow, a
			return
		}
		if mine && d.Unused.Text == "" {
			d.Unused, d.Why = a, why
		}
	}
}

// tooWide says an allow has a wildcard within its first word or right after
// it, as Bash(rm *) and Bash(rm:*) do.
func tooWide(allow string) bool {
	cmd, wild, ok := permissions.BashCommand(allow)
	if !ok {
		return true
	}
	if !wild {
		return false
	}
	lit, _, _ := strings.Cut(cmd, "*")
	return len(strings.Fields(lit)) < 2
}

// PreApproves lists the rules an allow in aggy's own file names or covers,
// whether or not a launch takes it as a pre-approval: suggest leaves out a
// command that names a strict one.
func PreApproves(allow string, branches []string) []PolicyRule {
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
// allow"). A pre-approved rule is left out, and Claude's own prompt stands for
// what it covered.
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
// grok-build 2bdd1d6a): every rule is a deny there, never an allow, and a
// pre-approval removes none, as Grok has no prompt of its own behind a removed
// rule (it matches an allow as a word-boundary prefix, and reads every file).
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
// (verification.md, "OpenCode 2.2.5"), every ask is a deny. A pre-approval
// removes none. The strict rules stay denies: OpenCode may keep an Always
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
