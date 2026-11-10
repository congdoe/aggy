package parity

import (
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/permissions"
)

// One tool call judged against the safety policy (T6976, docs/design.md,
// section 10, "Exact pre-approval"). A vendor's tool-use hook calls aggy
// with the call it is about to make; this says allow, ask, deny, or nothing.
// It is the one decision for every CLI whose hook can ask aggy: Claude's
// PreToolUse hook and OpenCode's permission evaluate hook.
//
// What counts as a policy match is the static rules' own matcher (glob, which
// Names and covers use), applied more widely than a CLI applies a rule: at
// every word of the command, in three spellings, so the hook never misses a
// command a static rule would have asked about. Where it is wider, it asks
// more, never less.

// CallKind is what a tool call does.
type CallKind string

const (
	// CallShell runs a shell command: Text is the command, byte for byte.
	CallShell CallKind = "shell"
	// CallRead reads one file: Text is its path as the tool was given it.
	CallRead CallKind = "read"
	// CallWrite writes or edits one file. A read pre-approval never covers it.
	CallWrite CallKind = "write"
	// CallSearch is a search tool's path or pattern, which may name many
	// files. Nothing pre-approves one.
	CallSearch CallKind = "search"
)

// Call is one tool call to judge.
type Call struct {
	Kind CallKind
	Text string
	// Other is a call from a tool that is not the one a pre-approval is for
	// (an MCP tool with a command, say): it is never allowed, and what would
	// be an allow is no opinion, so the CLI's own flow decides (PR #129
	// re-review).
	Other bool
}

// Verdict is what the hook answers.
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictAsk   Verdict = "ask"
	VerdictDeny  Verdict = "deny"
	// VerdictNone is no opinion: the CLI's own permission flow decides.
	VerdictNone Verdict = "no-opinion"
)

// Reason says why, as a fixed word; internal/cli maps it to a diag code.
type Reason string

const (
	ReasonNoMatch      Reason = "no-match"      // no policy rule matches
	ReasonPreApproved  Reason = "pre-approved"  // an exact pre-approval of the whole call
	ReasonPolicyMatch  Reason = "policy-match"  // a rule matches and nothing pre-approves the call
	ReasonGroupDenied  Reason = "group-denied"  // the person set a matching group to deny
	ReasonNoPrompt     Reason = "no-prompt"     // an ask in a session that cannot show a prompt
	ReasonUnknownInput Reason = "unknown-input" // a call aggy cannot read: asked, never allowed
	ReasonTooLong      Reason = "too-long"      // a call longer than MaxCallText: asked at once, never allowed
	ReasonProtected    Reason = "protected"     // a call that names aggy's own files or a Claude settings file: asked
	ReasonPlanChanged  Reason = "plan-changed"  // the launch's plan is missing, unreadable, or not what the launch wrote: asked
	ReasonOtherTool    Reason = "other-tool"    // pre-approved text from a tool the pre-approval is not for: no opinion
)

// MaxCallText caps what the matcher scans of one call. A longer one asks at
// once (ReasonTooLong): the matcher tries every rule at every word in three
// spellings, which takes seconds on a large command, past the hook's timeout.
const MaxCallText = 64 << 10

// CallDecision is the decision on one call. Group is the first matching
// rule's group, "" when none matches.
type CallDecision struct {
	Verdict Verdict
	Group   string
	Strict  bool
	Reason  Reason
}

// PreApproved are the person's exact pre-approvals for one launch: commands,
// matched whole and byte for byte, and files, each by its cleaned absolute
// path and the identity it had when the person added it.
type PreApproved struct {
	Commands []string
	Reads    []ReadApproval
	// CheckRead says the file a read names is still the one approved: the
	// path as given, with no link on the way, and the same identity. Nil
	// allows no read (internal/policyhook sets it, as it reads the disk).
	CheckRead func(ReadApproval) bool
}

// ReadApproval is one exact read pre-approval: the cleaned absolute path and
// the file's device and inode when aggy permissions add recorded them, so a
// file replaced, or a link put in its place, asks again (PR #129 review).
type ReadApproval struct {
	Path string `json:"path"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

// ReadIdentity is the identity aggy recorded for an exact read
// pre-approval's path, from the index; ok is false when there is none.
type ReadIdentity func(path string) (dev, ino uint64, ok bool)

// PreApprovedFrom collects the exact pre-approvals in s: allows from aggy's
// own permissions file only (permissions.FromAggy and FromAggyRepo), which
// aggy permissions add writes at a terminal. A rule from a Claude settings
// file, the repo, aggy's config, or a CLI's saved approvals never
// pre-approves a policy match, and nor does any allow while s withholds its
// allows. A Bash allow counts when it has no wildcard and passes ExactRule; a
// Read allow when it names one file, with no wildcard: //abs is absolute, and
// ./rel or a bare relative path is under root, the launch's folder (a path
// starting with / or ~ is Claude's relative-to-settings or home form, which
// aggy's filter never passes for a key file, and is left out). A read counts
// only in a trusted repo (s.Trusted), and only with the identity ids recorded
// for it (PR #129 review): a read allow with none pre-approves nothing.
func PreApprovedFrom(s permissions.Set, root string, ids ReadIdentity) PreApproved {
	var out PreApproved
	if s.Withhold != "" || s.FileErr != nil {
		return out
	}
	for _, r := range s.Rules {
		if r.Kind != permissions.Allow || (r.Origin != permissions.FromAggy && r.Origin != permissions.FromAggyRepo) {
			continue
		}
		if cmd, ok := ExactCommand(r.Text); ok {
			if !slices.Contains(out.Commands, cmd) {
				out.Commands = append(out.Commands, cmd)
			}
			continue
		}
		p, ok := ExactRead(r.Text, root)
		if !ok || !s.Trusted || ids == nil || slices.ContainsFunc(out.Reads, func(a ReadApproval) bool { return a.Path == p }) {
			continue
		}
		if dev, ino, ok := ids(p); ok {
			out.Reads = append(out.Reads, ReadApproval{Path: p, Dev: dev, Ino: ino})
		}
	}
	return out
}

// ExactReads are the paths of s's exact read allows from aggy's own file,
// whatever their trust or identity, for aggy permissions to name.
func ExactReads(s permissions.Set, root string) []string {
	var out []string
	for _, r := range s.Rules {
		if r.Kind != permissions.Allow || (r.Origin != permissions.FromAggy && r.Origin != permissions.FromAggyRepo) {
			continue
		}
		if p, ok := ExactRead(r.Text, root); ok && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// ExactCommand returns the command an allow rule pre-approves exactly: a
// Bash rule with no wildcard whose command ExactRule takes.
func ExactCommand(rule string) (string, bool) {
	cmd, wild, ok := permissions.BashCommand(rule)
	if !ok || wild || ExactRule(cmd) != nil {
		return "", false
	}
	return cmd, true
}

// ExactRead returns the absolute path a Read allow names exactly.
func ExactRead(rule, root string) (string, bool) {
	tool, spec, has, err := permissions.ParseRule(rule)
	if err != nil || tool != "Read" || !has || spec == "" || strings.ContainsAny(spec, "*?[{\n\r") {
		return "", false
	}
	var p string
	switch {
	case strings.HasPrefix(spec, "//"):
		p = spec[1:]
	case strings.HasPrefix(spec, "/"), strings.HasPrefix(spec, "~"):
		return "", false
	default:
		if root == "" || !filepath.IsAbs(root) {
			return "", false
		}
		p = filepath.Join(root, strings.TrimPrefix(spec, "./"))
	}
	if filepath.Clean(p) != p || !filepath.IsAbs(p) {
		return "", false
	}
	return p, true
}

// DecideCall judges one call by the launch's plan (an on-screen plan: a
// headless one makes every rule a deny) and the person's exact pre-approvals:
//
//   - no rule matches: no opinion;
//   - a matching rule's group is set to deny: deny;
//   - the whole call is exactly pre-approved: allow;
//   - otherwise: ask.
//
// It reads no file and no environment, so nothing but its arguments decides.
func (p PolicyPlan) DecideCall(c Call, pre PreApproved) CallDecision {
	if len(c.Text) > MaxCallText {
		return CallDecision{Verdict: VerdictAsk, Reason: ReasonTooLong}
	}
	var matched []PolicyDecision
	for _, d := range p.Decide() {
		if callMatches(c, d.PolicyRule) {
			matched = append(matched, d)
		}
	}
	if len(matched) == 0 {
		return CallDecision{Verdict: VerdictNone, Reason: ReasonNoMatch}
	}
	strict := slices.ContainsFunc(matched, func(d PolicyDecision) bool { return d.Strict })
	if i := slices.IndexFunc(matched, func(d PolicyDecision) bool { return d.Kind == permissions.Deny }); i >= 0 {
		return CallDecision{Verdict: VerdictDeny, Group: matched[i].Group, Strict: strict, Reason: ReasonGroupDenied}
	}
	out := CallDecision{Verdict: VerdictAsk, Group: matched[0].Group, Strict: strict, Reason: ReasonPolicyMatch}
	if preApproved(c, pre) {
		out.Verdict, out.Reason = VerdictAllow, ReasonPreApproved
		if c.Other {
			out.Verdict, out.Reason = VerdictNone, ReasonOtherTool
		}
	}
	return out
}

// preApproved is a byte-for-byte match of the whole call: no trimming, no
// folding, no part of a compound command. A read must name the approved path
// exactly, absolute and clean, and pass CheckRead.
func preApproved(c Call, pre PreApproved) bool {
	switch c.Kind {
	case CallShell:
		return c.Text != "" && slices.Contains(pre.Commands, c.Text)
	case CallRead:
		if c.Text == "" || pre.CheckRead == nil {
			return false
		}
		i := slices.IndexFunc(pre.Reads, func(a ReadApproval) bool { return a.Path == c.Text })
		return i >= 0 && pre.CheckRead(pre.Reads[i])
	}
	return false
}

// MatchedGroups lists the groups whose on-screen rules match c, once each,
// in the policy's order: for aggy permissions, which names what an allow
// pre-approves.
func MatchedGroups(c Call, branches []string) (groups []string, strict bool) {
	for _, r := range PolicyRules(true, branches) {
		if callMatches(c, r) {
			if !slices.Contains(groups, r.Group) {
				groups = append(groups, r.Group)
			}
			strict = strict || r.Strict
		}
	}
	return groups, strict
}

// callMatches says rule r matches call c.
func callMatches(c Call, r PolicyRule) bool {
	tool, spec, _, ok := ruleParts(r.Text)
	if !ok {
		return false
	}
	switch {
	case tool == "Bash" && c.Kind == CallShell:
		return shellMatches(c.Text, spec)
	case tool == "Read" && c.Kind == CallShell:
		// Claude applies a Read rule to the file commands it recognizes in a
		// shell (cat .env); any word naming the file counts here.
		return slices.ContainsFunc(shellForms(c.Text), func(f string) bool { return namesFile(f, path.Base(spec)) })
	case tool == "Read" && (c.Kind == CallRead || c.Kind == CallWrite):
		return fileMatches(c.Text, path.Base(spec))
	case tool == "Read" && c.Kind == CallSearch:
		return searchMatches(c.Text, path.Base(spec))
	}
	return false
}

// shellForms are the spellings of a command the matcher reads: as given, with
// line continuations joined, and with quote marks and backslashes gone, each
// with its whitespace collapsed. A shell would run each the same way, so a
// quote or a continuation inside a word never hides it from a rule.
func shellForms(cmd string) []string {
	joined := strings.ReplaceAll(cmd, "\\\n", "")
	bare := strings.NewReplacer(`"`, "", "'", "", `\`, "").Replace(joined)
	var out []string
	for _, f := range []string{cmd, joined, bare} {
		f = strings.Join(strings.Fields(f), " ")
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// wordStart is a byte after which a new word, and so a command, may start.
func wordStart(b byte) bool {
	return strings.IndexByte(" \t;&|(){}`!<>/\\=$", b) >= 0
}

// shellMatches says a Bash rule's command, read as a prefix, matches the
// command at any word: Claude applies a rule to each part of a compound
// command (permissions.md, "Compound commands"), and a part may start after
// any operator, a subshell, a substitution, or a program's path.
func shellMatches(cmd, rule string) bool {
	pattern := rule
	if !strings.HasSuffix(pattern, "*") {
		pattern += "*"
	}
	for _, f := range shellForms(cmd) {
		for i := 0; i < len(f); i++ {
			if (i == 0 || wordStart(f[i-1])) && glob(pattern, f[i:]) {
				return true
			}
		}
	}
	return false
}

// shellWordSplit are the bytes a shell word ends at, for finding file names.
const shellWordSplit = " \t\n;&|(){}<>`=,:\"'"

// namesFile says a word of the command names a file called name, by its last
// path element, whatever its case (a file system that folds case opens .ENV
// as .env).
func namesFile(cmd, name string) bool {
	words := strings.FieldsFunc(cmd, func(r rune) bool { return r < 0x80 && strings.ContainsRune(shellWordSplit, r) })
	for _, w := range words {
		if fileMatches(w, name) {
			return true
		}
	}
	return false
}

// fileMatches says a path's last element is name, a policy file name such as
// .env or *.pem, whatever its case.
func fileMatches(p, name string) bool {
	base := p
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		base = p[i+1:]
	}
	return base != "" && glob(strings.ToLower(name), strings.ToLower(base))
}

// searchMatches says a search tool's path or pattern may name the file: by
// its last element, or with its wildcards taken out, holding the name's
// fixed text (**/*.pem and .env* both count).
func searchMatches(text, name string) bool {
	if fileMatches(text, name) {
		return true
	}
	fixed := strings.Trim(strings.ToLower(name), "*")
	bare := strings.NewReplacer("*", "", "?", "", "[", "", "]", "", "{", "", "}", "").Replace(strings.ToLower(text))
	return fixed != "" && strings.Contains(bare, fixed)
}
