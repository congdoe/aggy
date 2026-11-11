package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/suggest"
	"github.com/congdoe/aggy/internal/termsafe"
)

const permissionsUsage = `Usage: aggy permissions <command>

One set of rules for every CLI, in Claude Code's format. Rules take effect at
the next launch or switch, and a deny always wins.

  list                       List the rules for this repo, with each one's source
  show                       The same, plus what each CLI gets, and what was
                             skipped and why
  add <allow|ask|deny> RULE  Add a rule for this repo, or --everywhere
  remove <allow|ask|deny> RULE
                             Remove one of aggy's rules, for this repo or --everywhere
  suggest [--all]            Propose rules from commands you ran in Claude Code,
                             in this repo or, with --all, every repo; you confirm each
  policy [GROUP ask|deny|allow]
                             List the safety policy's groups, or set one to ask
                             (the default on screen), deny, or allow (off on
                             screen), for this repo or --everywhere

  --everywhere   the rule holds in every repo, not only this one
  --dry-run      show the change and make none

A rule looks like Bash(git status), Bash(git diff *), Read(./docs/**), or
WebFetch(domain:example.com). Rules aggy owns live in ~/.aggy/permissions.json,
which you can also edit by hand; aggy doctor reports a mistake by line. A rule
from Claude's own files stays there: change it with /permissions in Claude.

Allows aggy passes on are filtered much as Claude filters them entering auto mode:
no blanket shell access, no wildcarded interpreters, no wildcarded run commands.
In a repo you have not trusted (aggy trust), only known read-only forms apply, such
as git status and ls, and anything else is held and named. A repo's committed rules
and .aggy/config.toml never become allows. Claude and Grok still apply what they
read from their own files themselves, whatever aggy's filter says.

The safety policy (aggy policy --help) asks on screen where a CLI can ask. Only
one thing lifts that ask: an exact pre-approval you add here, such as
add allow "Bash(git push --force origin main)". It runs without asking on
Claude Code, and on OpenCode, when the command is exactly that, whole; any other
spelling, or the command joined to another with &&, ;, or a pipe, still asks. A
wildcard rule, a rule in Claude's or any CLI's own files, or anything in the
repo never pre-approves a policy match. Codex and Grok Build are not changed by
one (Codex asks, Grok Build refuses). Two groups are strict: a force push to
main, master, trunk, or the default branch, and reading private key files; add
says what such a pre-approval allows before it asks you, OpenCode lifts them
only in a repo trusted for it without --auto, and suggest never proposes them.
list marks them. A deny, yours or a group set to deny, always wins over an allow.

policy GROUP allow turns one group off on screen: Claude Code, Grok Build, and
OpenCode get no ask or deny for it, and their own rules decide. It is asked at a
terminal, with a plain warning for the two strict groups, and is only ever set
in your ~/.aggy/permissions.json, never by a repo. It stays on for Codex (its
rules file is global), for a borrow child, a headless run, and print mode
(refused there), and aggy's asks on writes to its own files and Claude's
settings files stay. Each launch says which groups are off.

Changing rules is for you, at a terminal: a session's tool shell and aggy unwire
from one are refused. That holds against mistakes, not a model that tries: it can
give itself a terminal with script, and pipe y.
`

// permFail records a refusal of aggy permissions and exits 1.
func permFail(stderr io.Writer, cmd string, code diag.Code, msg string, dryRun bool) int {
	refuseWith(writerFor(dryRun), context.Background(), stderr, cmd, diag.EvPermissionsRefused, code, msg, diag.Rec{})
	return 1
}

func runPermissions(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, permissionsUsage)
		return 0
	}
	switch args[0] {
	case "list":
		return permissionsList(args[1:], stdout, stderr)
	case "show":
		return permissionsShow(args[1:], stdout, stderr)
	case "add":
		return permissionsChange(args[1:], false, stdout, stderr)
	case "remove":
		return permissionsChange(args[1:], true, stdout, stderr)
	case "suggest":
		return permissionsSuggest(args[1:], stdout, stderr)
	case "policy":
		return permissionsPolicy(args[1:], stdout, stderr)
	}
	return cmdUsage(stderr, "permissions", fmt.Sprintf("unknown command %q", args[0]), permissionsUsage)
}

// originName says where a rule came from, in a person's words.
func originName(o permissions.Origin) string {
	switch o {
	case permissions.FromClaudeUser:
		return "Claude, your settings"
	case permissions.FromClaudeLocal:
		return "Claude, this repo's local settings"
	case permissions.FromRepoLocal:
		return "Claude, this repo's committed local settings"
	case permissions.FromAggy:
		return "aggy, everywhere"
	case permissions.FromAggyRepo:
		return "aggy, this repo"
	case permissions.FromConfig:
		return "aggy's config"
	case permissions.FromGrokGrant:
		return "approved in Grok, this repo"
	case permissions.FromCodexGrant:
		return "approved in Codex"
	}
	return string(o)
}

// appliedBy says a rule comes from a CLI's own file, so that CLI applies it
// whatever aggy does with it.
func appliedBy(o permissions.Origin) string {
	switch o {
	case permissions.FromClaudeUser, permissions.FromClaudeLocal, permissions.FromRepoLocal:
		return " Claude applies this itself"
	case permissions.FromGrokGrant:
		return " Grok applies this itself"
	case permissions.FromCodexGrant:
		return " Codex applies this itself"
	}
	return ""
}

// permissionsHere reads the rules for the current folder.
func permissionsHere(ctx context.Context) (permissions.Set, permissions.Input, config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return permissions.Set{}, permissions.Input{}, config.Config{}, err
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return permissions.Set{}, permissions.Input{}, config.Config{}, err
	}
	in := permissionInput(cwd)
	return loadPermissions(ctx, cwd, cfg), in, cfg, nil
}

func permissionsList(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return cmdUsage(stderr, "permissions list", "takes no arguments", permissionsUsage)
	}
	s, in, _, err := permissionsHere(context.Background())
	if err != nil {
		return cmdErr(stderr, "permissions list", diag.CodeConfig, err)
	}
	printPermissionsHeader(stdout, s, in)
	printRules(stdout, s)
	if cwd, err := os.Getwd(); err == nil {
		var legacy func(string) bool
		ctx := context.Background()
		if db, _, err := openIndexReadOnly(ctx); err == nil && db != nil {
			defer func() { _ = db.Close() }()
			legacy = func(p string) bool {
				a, ok, err := db.ReadApprovalFor(ctx, p)
				return ok && err == nil && !a.Complete
			}
		}
		printPreApprovals(stdout, s, cwd, legacy)
	}
	if n := len(s.Skipped); n > 0 {
		fmt.Fprintf(stdout, "\n%d rules do not apply. aggy permissions show says why.\n", n)
	}
	printPermissionsTroubles(stdout, s)
	return 0
}

// printPermissionsTroubles says what a launch would say or refuse on.
func printPermissionsTroubles(w io.Writer, s permissions.Set) {
	if s.FileErr != nil {
		fmt.Fprintf(w, "\nA launch refuses to start until this is fixed: %s\n", termsafe.Line(s.FileErr.Error()))
	}
	for _, p := range s.Problems {
		fmt.Fprintf(w, "\nA problem with %s: %s\n", termsafe.Field(p.Path), termsafe.Line(p.Why))
	}
	if s.Withhold != "" {
		fmt.Fprintf(w, "\n%s\n", termsafe.Line(s.Withhold))
	}
	for _, n := range s.Notes {
		fmt.Fprintf(w, "\n%s\n", termsafe.Line(n))
	}
}

func printPermissionsHeader(w io.Writer, s permissions.Set, in permissions.Input) {
	switch {
	case in.Main == "":
		fmt.Fprintln(w, "Not in a git repo: only rules that hold everywhere apply here.")
	case in.Trusted:
		fmt.Fprintf(w, "Repo %s (trusted)\n", termsafe.Field(in.Main))
	default:
		fmt.Fprintf(w, "Repo %s (not trusted: aggy trust <dir>)\n", termsafe.Field(in.Main))
	}
	if s.Mode != "" {
		fmt.Fprintf(w, "Mode: %s (%s)\n", termsafe.Field(s.Mode), originName(s.ModeFrom))
	} else {
		fmt.Fprintln(w, "Mode: none set; each CLI starts in its own default")
	}
}

// printRules lists rules in the order Claude evaluates them.
func printRules(w io.Writer, s permissions.Set) {
	for _, k := range permissions.Kinds {
		fmt.Fprintf(w, "\n%s\n", k)
		n := 0
		for _, r := range s.Rules {
			if r.Kind == k {
				fmt.Fprintf(w, "  %s  [%s]%s\n", termsafe.Field(r.Text), originName(r.Origin), appliedBy(r.Origin))
				n++
			}
		}
		if n == 0 {
			fmt.Fprintln(w, "  (none)")
		}
	}
}

// permissionsChange is aggy permissions add and remove.
func permissionsChange(args []string, remove bool, stdout, stderr io.Writer) int {
	name := "permissions add"
	if remove {
		name = "permissions remove"
	}
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	everywhere := fl.Bool("everywhere", false, "")
	dryRun := fl.Bool("dry-run", false, "")
	var pos []string
	// Flags may follow the rule, which is how people write it.
	for rest := args; ; {
		if err := fl.Parse(rest); err != nil {
			return cmdUsage(stderr, name, err.Error(), permissionsUsage)
		}
		if fl.NArg() == 0 {
			break
		}
		pos = append(pos, fl.Arg(0))
		rest = fl.Args()[1:]
	}
	if len(pos) != 2 || !slices.Contains(permissions.Kinds, permissions.Kind(pos[0])) {
		return cmdUsage(stderr, name, "want allow, ask, or deny, and one quoted rule", permissionsUsage)
	}
	kind, rule := permissions.Kind(pos[0]), pos[1]
	ctx := context.Background()
	fail := func(code diag.Code, msg string) int { return permFail(stderr, name, code, msg, *dryRun) }
	if _, _, _, err := permissions.ParseRule(rule); err != nil {
		return fail(diag.CodePermBadRule, err.Error())
	}
	if kind == permissions.Allow && !remove {
		if class, why := permissions.CheckAllow(rule); class == permissions.Drop {
			return fail(diag.CodePermBadRule, "this allow would never be passed on: "+why)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	in := permissionInput(cwd)
	repoKey := in.RepoKey
	if *everywhere {
		repoKey = ""
	} else if repoKey == "" {
		return fail(diag.CodePermScope, "this folder is in no git repo, so name --everywhere")
	}
	path, err := permissionsFile()
	if err != nil {
		return fail(diag.CodeStore, err.Error())
	}
	src, hash, err := readPermissionsFile(path)
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	has, err := permissions.Has(src, kind, rule, repoKey)
	if err != nil {
		return fail(diag.CodePermBadFile, fmt.Sprintf("%s is not valid: %s; aggy doctor says where", path, err))
	}
	where := "this repo"
	if repoKey == "" {
		where = "every repo"
	}
	switch {
	case !remove && has:
		fmt.Fprintf(stdout, "%s is already a %s rule for %s.\n", termsafe.Field(rule), kind, where)
		return 0
	case remove && !has:
		return fail(diag.CodePermNotFound, notAggysRule(ctx, kind, rule, where))
	}
	edit := permissions.Edit{Kind: kind, Rule: rule, RepoKey: repoKey, Remove: remove}
	ps := []patch.Patch{permissionsPatch(path, hash, edit)}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	if err := personGate(os.Getenv); err != nil {
		return fail(diag.CodeNotAtKeyboard, err.Error())
	}
	verb := "Add"
	if remove {
		verb = "Remove"
	}
	// An exact read pre-approval records the file's identity (PR #129 review,
	// MEDIUM-4), so a file replaced or a link put in its place asks again.
	read := readPreApproval(rule, cwd, kind)
	if kind == permissions.Allow && !remove {
		if w := strictWarning(rule, policyMatch(rule, cwd)); w != "" {
			fmt.Fprintln(stdout, w)
		}
		if read.path != "" && read.err != nil {
			fmt.Fprintf(stdout, "It will pre-approve nothing of the safety policy: %s is %s. Add it again once the file is there.\n", termsafe.Field(read.path), termsafe.Line(read.err.Error()))
		}
	}
	if !confirmed(stdout, fmt.Sprintf("%s the %s rule %s for %s?", verb, kind, termsafe.Field(rule), where)) {
		fmt.Fprintln(stdout, "Nothing changed.")
		return 1
	}
	runID, err := applyPatches(ctx, ps)
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	fmt.Fprintf(stdout, "Done (run %s). It applies at the next launch or switch. aggy unwire --run %s undoes it, once any change made after it is undone.\n", runID, runID)
	if read.path != "" && (remove || read.err == nil) {
		if err := recordReadApproval(ctx, read, remove); err != nil {
			fmt.Fprintf(stdout, "aggy could not record the file's identity, so it pre-approves nothing yet: %s\n", termsafe.Line(err.Error()))
		}
	}
	if read.path != "" && !remove && read.err == nil {
		fmt.Fprintln(stdout, "A read pre-approval applies only in a repo you trusted (aggy trust), and only while the file is this same file at this path, with no link on the way: a replaced file asks again.")
	}
	if kind == permissions.Allow && !remove {
		var policy map[string]permissions.Kind
		if s, _, _, err := permissionsHere(ctx); err == nil {
			policy = s.Policy
		}
		if line := policyCoveredLine(rule, cwd, policy); line != "" {
			fmt.Fprintln(stdout, line)
		}
	}
	if kind == permissions.Allow && !remove && !in.Trusted {
		if class, why := permissions.CheckAllow(rule); class == permissions.RepoCode {
			fmt.Fprintf(stdout, "It applies only in a repo you trusted: %s. aggy trust <dir> does that.\n", termsafe.Line(why))
		}
	}
	return 0
}

// readApprovalIn is an exact Read allow's file: its path, and its identity or
// why there is none.
type readApprovalIn struct {
	path string
	id   parity.ReadApproval
	err  error
}

// readPreApproval reads the file an exact Read allow names, from cwd's repo
// root: path is "" for any other rule.
func readPreApproval(rule, cwd string, kind permissions.Kind) readApprovalIn {
	if kind != permissions.Allow {
		return readApprovalIn{}
	}
	root, _ := repoRoot(cwd)
	p, ok := parity.ExactRead(rule, root)
	if !ok {
		return readApprovalIn{}
	}
	if root == "" {
		return readApprovalIn{path: p, err: errors.New("not in a git repo")}
	}
	id, err := policyhook.FileIdentity(root, p)
	return readApprovalIn{path: p, id: id, err: err}
}

// recordReadApproval records, or with remove forgets, a read pre-approval's
// identity in the index.
func recordReadApproval(ctx context.Context, r readApprovalIn, remove bool) error {
	if remove {
		s, err := openIndex(ctx)
		if err != nil || s == nil {
			return err
		}
		defer func() { _ = s.Close() }()
		return s.DeleteReadApproval(ctx, r.path)
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	id := r.id
	return s.PutReadApproval(ctx, store.ReadApproval{Path: r.path, Dev: id.Dev, Ino: id.Ino, Size: id.Size, ModNS: id.ModNS, ChangeNS: id.ChangeNS, At: time.Now()})
}

// preApproval is what an allow rule pre-approves of the safety policy: the
// groups it matches and whether one is strict. exact is false for a rule that
// names or covers policy rules without being one exact command or file, which
// pre-approves nothing.
type preApproval struct {
	groups []string
	strict bool
	exact  bool
}

// policyMatch reads an allow rule against the policy, by the decision hook's
// own matcher for an exact command or file (parity.MatchedGroups), and by the
// rules it names or covers otherwise (parity.CoveredRules).
func policyMatch(rule, cwd string) preApproval {
	branches, _ := defaultBranches(cwd)
	root, _ := repoRoot(cwd)
	if cmd, ok := parity.ExactCommand(rule); ok {
		g, strict := parity.MatchedGroups(parity.Call{Kind: parity.CallShell, Text: cmd}, branches)
		return preApproval{g, strict, len(g) > 0}
	}
	if p, ok := parity.ExactRead(rule, root); ok {
		g, strict := parity.MatchedGroups(parity.Call{Kind: parity.CallRead, Text: p}, branches)
		return preApproval{g, strict, len(g) > 0}
	}
	var out preApproval
	for _, r := range parity.CoveredRules(rule, branches) {
		if !slices.Contains(out.groups, r.Group) {
			out.groups = append(out.groups, r.Group)
		}
		out.strict = out.strict || r.Strict
	}
	return out
}

// strictWarning is what aggy permissions add says, before it asks, about an
// exact pre-approval of a strict group.
func strictWarning(rule string, m preApproval) string {
	if !m.exact || !m.strict {
		return ""
	}
	return fmt.Sprintf("%s pre-approves a strict safety policy case (%s): a force push to a protected branch, or reading a private key file. On Claude Code, and on OpenCode in a repo trusted for it without --auto, it will run with no prompt each time it is run exactly as written here. Any other spelling, or the same command joined to another, still asks.",
		termsafe.Field(rule), strings.Join(m.groups, ", "))
}

// policyCoveredLine is the line aggy permissions add prints for an allow that
// matches, names, or covers safety policy rules, or "": what it does per CLI
// now (T6976). Only an exact command or file pre-approves, and only on the CLIs
// whose hook can ask aggy: Claude Code and OpenCode. A group set to deny refuses
// it everywhere.
func policyCoveredLine(rule, cwd string, policy map[string]permissions.Kind) string {
	m := policyMatch(rule, cwd)
	if len(m.groups) == 0 {
		return ""
	}
	groups := strings.Join(m.groups, ", ")
	for _, g := range m.groups {
		if policy[g] == permissions.Deny {
			return fmt.Sprintf("This matches aggy's safety policy (%s), and %s is set to deny here, so every CLI refuses it whatever you allow. aggy permissions policy %s ask sets it back to asking.", groups, g, g)
		}
	}
	if !slices.ContainsFunc(m.groups, func(g string) bool { return policy[g] != permissions.Allow }) {
		return fmt.Sprintf("This matches aggy's safety policy (%s), which you turned off here, so the policy adds nothing to it but on Codex, which still asks.", groups)
	}
	const other = " Codex and Grok Build: no effect on policy matches (Codex asks, Grok Build refuses)."
	if !m.exact {
		return fmt.Sprintf("This covers aggy's safety policy (%s), but only one exact command or file pre-approves a policy match, so what it covers still asks on Claude Code and OpenCode (refused under OpenCode --auto).%s aggy permissions policy <group> deny refuses it instead.", groups, other)
	}
	oc := "OpenCode: runs without asking, except under --auto, where it is refused."
	if m.strict {
		oc = "OpenCode: runs without asking in a repo trusted for OpenCode without --auto; refused otherwise."
	}
	return fmt.Sprintf("This pre-approves an exact match of aggy's safety policy (%s). Claude Code: runs without asking, when run exactly as written. %s%s Any other spelling still asks.", groups, oc, other)
}

// printPreApprovals lists the exact pre-approvals of policy matches in s, the
// strict ones marked. legacy, which may be nil, reports a read pre-approval
// whose recorded identity predates the size and times aggy now compares: it
// matches no file, so the line says it asks until the rule is added again.
func printPreApprovals(w io.Writer, s permissions.Set, cwd string, legacy func(path string) bool) {
	root, _ := repoRoot(cwd)
	var lines []string
	for _, r := range s.Rules {
		if r.Kind != permissions.Allow || (r.Origin != permissions.FromAggy && r.Origin != permissions.FromAggyRepo) {
			continue
		}
		m := policyMatch(r.Text, cwd)
		if !m.exact {
			continue
		}
		mark := ""
		if m.strict {
			mark = "  STRICT"
		}
		if p, ok := parity.ExactRead(r.Text, root); ok && legacy != nil && legacy(p) {
			mark += "  (added before aggy recorded enough of the file to tell a replaced one: asks until you add it again)"
		}
		lines = append(lines, fmt.Sprintf("  %s  [%s] %s%s", termsafe.Field(r.Text), originName(r.Origin), strings.Join(m.groups, ", "), mark))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "\nExact pre-approvals of the safety policy (run without asking on Claude Code and OpenCode)")
	if s.Withhold != "" {
		fmt.Fprintln(w, "  None apply until the problem below is fixed.")
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// permissionsPolicy is aggy permissions policy: with no arguments it lists the
// groups, and with a group and ask or deny it sets one, through the manifest.
func permissionsPolicy(args []string, stdout, stderr io.Writer) int {
	const name = "permissions policy"
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	everywhere := fl.Bool("everywhere", false, "")
	dryRun := fl.Bool("dry-run", false, "")
	var pos []string
	for rest := args; ; {
		if err := fl.Parse(rest); err != nil {
			return cmdUsage(stderr, name, err.Error(), permissionsUsage)
		}
		if fl.NArg() == 0 {
			break
		}
		pos = append(pos, fl.Arg(0))
		rest = fl.Args()[1:]
	}
	ctx := context.Background()
	if len(pos) == 0 {
		s, _, _, err := permissionsHere(ctx)
		if err != nil {
			return cmdErr(stderr, name, diag.CodeConfig, err)
		}
		printPolicy(stdout, s)
		return 0
	}
	if len(pos) != 2 {
		return cmdUsage(stderr, name, "want a group and ask, deny, or allow", permissionsUsage)
	}
	fail := func(code diag.Code, msg string) int { return permFail(stderr, name, code, msg, *dryRun) }
	group, setting := pos[0], pos[1]
	if err := permissions.CheckPolicySetting(group, setting); err != nil {
		return fail(diag.CodePermBadRule, err.Error())
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	repoKey := permissionInput(cwd).RepoKey
	if *everywhere {
		repoKey = ""
	} else if repoKey == "" {
		return fail(diag.CodePermScope, "this folder is in no git repo, so name --everywhere")
	}
	path, err := permissionsFile()
	if err != nil {
		return fail(diag.CodeStore, err.Error())
	}
	src, hash, err := readPermissionsFile(path)
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	cur, err := permissions.PolicySetting(src, group, repoKey)
	if err != nil {
		return fail(diag.CodePermBadFile, fmt.Sprintf("%s is not valid: %s; aggy doctor says where", path, err))
	}
	where := "this repo"
	if repoKey == "" {
		where = "every repo"
	}
	if cur == setting {
		fmt.Fprintf(stdout, "%s is already %s for %s.\n", group, setting, where)
		return 0
	}
	edit := permissions.PolicyEdit{Group: group, Setting: setting, RepoKey: repoKey}
	ps := []patch.Patch{{File: path, ExpectedHash: hash, Mode: 0o600, Transform: edit.Apply}}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	if err := personGate(os.Getenv); err != nil {
		return fail(diag.CodeNotAtKeyboard, err.Error())
	}
	if setting == string(permissions.Allow) {
		// T6979: turning a group off is asked in place, with its own words.
		yes, asked := askInPlace(os.Getenv, offerIn, stdout, policyOffQuestion(group, where))
		if !asked {
			return fail(diag.CodeNotAtKeyboard, "turning a policy group off asks you at a terminal, with stdin and stderr terminals; run it yourself there")
		}
		if !yes {
			fmt.Fprintln(stdout, "Nothing changed.")
			return 1
		}
	} else if !confirmed(stdout, fmt.Sprintf("Set the safety policy's %s to %s for %s?", group, setting, where)) {
		fmt.Fprintln(stdout, "Nothing changed.")
		return 1
	}
	runID, err := applyPatches(ctx, ps)
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	fmt.Fprintf(stdout, "Done (run %s). It applies at the next launch or switch. aggy unwire --run %s undoes it, once any change made after it is undone.\n", runID, runID)
	switch setting {
	case string(permissions.Deny):
		fmt.Fprintln(stdout, "Claude Code, Grok Build, and OpenCode now refuse these; Codex's rules file is global and still asks.")
	case string(permissions.Allow):
		fmt.Fprintf(stdout, "%s is off on screen on Claude Code, Grok Build, and OpenCode; Codex still asks, and borrows still refuse. aggy permissions policy %s ask turns it back on.\n", group, group)
	}
	return 0
}

// strictOffWarning is the plain-words warning for turning a strict group off.
var strictOffWarning = map[string]string{
	permissions.PolicyKeys:      "key-read is strict. Off, a session reads your private key files (id_rsa, *.pem, *.key and the like) with no prompt from aggy on Claude Code and Grok Build, and the model can then copy a key into a reply, a file, or a command.",
	permissions.PolicyForceMain: "force-push-main is strict. Off, a force push to main, master, trunk, or the default branch runs with no prompt from aggy on Claude Code, Grok Build, and OpenCode, and can erase work others pushed.",
}

// policyOffQuestion is the question aggy permissions policy GROUP allow asks.
//
// TODO(T6954): this question guards the command, not the file: a session's
// shell on Grok, which has no hook to ask about writes to aggy's files, can
// write a group set to allow into ~/.aggy/permissions.json directly (#133
// review).
func policyOffQuestion(group, where string) inPlace {
	change := fmt.Sprintf("This turns the safety policy's %s off for %s, in your ~/.aggy/permissions.json: %s then gets no ask or deny from aggy on Claude Code, Grok Build, and OpenCode, and their own rules decide. Codex still asks, and a borrow child, a headless run, and print mode still refuse it.", group, where, policyGroupNames[group])
	if w := strictOffWarning[group]; w != "" {
		change = w + "\n" + change
	}
	return inPlace{
		change: change,
		undo:   fmt.Sprintf("To undo: aggy permissions policy %s ask, or aggy unwire --run with the run id this prints.", group),
		ask:    fmt.Sprintf("Turn %s off for %s?", group, where),
	}
}

// notAggysRule words a remove of a rule aggy's file does not hold: a rule from
// a CLI's own file is not edited, since that CLI rewrites the file and
// internal/patch could not undo a change cleanly (docs/design.md, section 10).
func notAggysRule(ctx context.Context, k permissions.Kind, rule, where string) string {
	s, _, _, err := permissionsHere(ctx)
	if err == nil {
		for _, r := range s.Rules {
			if r.Kind != k || r.Text != rule {
				continue
			}
			switch r.Origin {
			case permissions.FromClaudeUser, permissions.FromClaudeLocal:
				return fmt.Sprintf("this rule is in %s, which aggy does not edit: change it with /permissions in Claude", r.Path)
			case permissions.FromGrokGrant, permissions.FromCodexGrant:
				return fmt.Sprintf("this rule is an approval in %s, which aggy does not edit: change it in that CLI", r.Path)
			case permissions.FromAggy:
				return "this rule holds everywhere: name --everywhere to remove it"
			case permissions.FromAggyRepo:
				return "this rule is for this repo: leave out --everywhere to remove it"
			}
		}
	}
	return fmt.Sprintf("aggy has no such %s rule for %s", k, where)
}

// readPermissionsFile returns the file and its hash, or none and NoFile.
func readPermissionsFile(path string) ([]byte, string, error) {
	b, err := safefs.ReadConfig(path, permissions.MaxFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, patch.NoFile, nil
	case err != nil:
		return nil, "", err
	}
	return b, patch.Hash(b), nil
}

func permissionsPatch(path, hash string, e permissions.Edit) patch.Patch {
	return patch.Patch{File: path, ExpectedHash: hash, Mode: 0o600, Transform: e.Apply}
}

// permissionsSuggest proposes commands from Claude's session files. Adding one
// takes the keyboard, every time: a count is a hint, not an approval.
func permissionsSuggest(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("permissions suggest", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	all := fl.Bool("all", false, "")
	if err := fl.Parse(args); err != nil || fl.NArg() > 0 {
		return cmdUsage(stderr, "permissions suggest", "bad arguments", permissionsUsage)
	}
	const name = "permissions suggest"
	fail := func(code diag.Code, msg string) int { return permFail(stderr, name, code, msg, false) }
	// The list is the model's commands as much as the person's, so it is shown
	// only to the person it asks.
	if err := personGate(os.Getenv); err != nil {
		return fail(diag.CodeNotAtKeyboard, err.Error())
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	in := permissionInput(cwd)
	if in.RepoKey == "" && !*all {
		return fail(diag.CodePermScope, "this folder is in no git repo; aggy permissions suggest --all covers every repo")
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return fail(diag.CodeStore, err.Error())
	}
	defer func() { _ = s.Close() }()
	if *all {
		scanAllRepos(ctx, s)
	} else {
		scanRepo(ctx, s, cwd)
	}
	repo := in.RepoKey
	if *all {
		repo = ""
	}
	cands, err := s.SuggestCandidates(ctx, repo, suggest.MinCount)
	if err != nil {
		return fail(diag.CodeStore, err.Error())
	}
	path, err := permissionsFile()
	if err != nil {
		return fail(diag.CodeStore, err.Error())
	}
	shown := 0
	for _, c := range cands {
		src, hash, err := readPermissionsFile(path)
		if err != nil {
			return fail(diag.CodePermBadFile, err.Error())
		}
		rule := "Bash(" + c.Command + ")"
		if has, err := permissions.Has(src, permissions.Allow, rule, c.Repo); err != nil {
			return fail(diag.CodePermBadFile, fmt.Sprintf("%s is not valid: %s; aggy doctor says where", path, err))
		} else if has {
			continue
		}
		// The rules may have changed, or the filter, since it was counted.
		if !suggest.Plain(c.Command) {
			continue
		}
		// A command that names a strict policy rule is never proposed.
		branches, _ := defaultBranches(c.Repo)
		if slices.ContainsFunc(parity.CoveredRules(rule, branches), func(r parity.PolicyRule) bool { return r.Strict }) {
			continue
		}
		shown++
		fmt.Fprintf(stdout, "%s\n  ran %d times in auto mode with no error", termsafe.Field(c.Command), c.Count)
		if *all {
			fmt.Fprintf(stdout, ", in %s", termsafe.Field(c.Repo))
		}
		fmt.Fprintln(stdout)
		if class, why := permissions.CheckAllow(rule); class == permissions.RepoCode {
			fmt.Fprintf(stdout, "  applies only in a trusted repo: %s\n", termsafe.Line(why))
		}
		if !confirmed(stdout, "  Allow it here from now on?") {
			if err := s.DismissSuggest(ctx, c.Repo, c.Command); err != nil {
				return fail(diag.CodeStore, err.Error())
			}
			continue
		}
		edit := permissions.Edit{Kind: permissions.Allow, Rule: rule, RepoKey: c.Repo}
		runID, err := applyPatches(ctx, []patch.Patch{permissionsPatch(path, hash, edit)})
		if err != nil {
			return fail(diag.CodePermBadFile, err.Error())
		}
		fmt.Fprintf(stdout, "  Added (run %s). It applies at the next launch or switch.\n", runID)
	}
	if shown == 0 {
		fmt.Fprintf(stdout, "Nothing new to propose. A command is proposed once it has run %d times in auto mode with no error.\n", suggest.MinCount)
	}
	return 0
}
