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

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/permissions"
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

  --everywhere   the rule holds in every repo, not only this one
  --dry-run      show the change and make none

A rule looks like Bash(git status), Bash(git diff *), Read(./docs/**), or
WebFetch(domain:example.com). Rules aggy owns live in ~/.aggy/permissions.json,
which you can also edit by hand; aggy doctor reports a mistake by line. A rule
from Claude's own files stays there: change it with /permissions in Claude.

Allows are filtered as Claude filters them entering auto mode: no blanket shell
access, no wildcarded interpreters, no package-manager run commands. An allow
that runs a repo's own code applies only in a repo you trusted (aggy trust).
A repo's committed rules and .aggy/config.toml never become allows.

Changing rules is for you, at a terminal: a session's tool shell is refused.
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
	if n := len(s.Skipped); n > 0 {
		fmt.Fprintf(stdout, "\n%d rules do not apply. aggy permissions show says why.\n", n)
	}
	for _, p := range s.Problems {
		fmt.Fprintf(stdout, "\nNot read: %s: %s\n", termsafe.Field(p.Path), termsafe.Line(p.Why))
	}
	return 0
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
				fmt.Fprintf(w, "  %s  [%s]\n", termsafe.Field(r.Text), originName(r.Origin))
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
	if !confirmed(stdout, fmt.Sprintf("%s the %s rule %s for %s?", verb, kind, termsafe.Field(rule), where)) {
		fmt.Fprintln(stdout, "Nothing changed.")
		return 1
	}
	runID, err := applyPatches(ctx, ps)
	if err != nil {
		return fail(diag.CodePermBadFile, err.Error())
	}
	fmt.Fprintf(stdout, "Done (run %s). It applies at the next launch or switch. aggy unwire --run %s undoes it, once any change made after it is undone.\n", runID, runID)
	if kind == permissions.Allow && !remove && !in.Trusted {
		if class, why := permissions.CheckAllow(rule); class == permissions.RepoCode {
			fmt.Fprintf(stdout, "It applies only in a repo you trusted: %s. aggy trust <dir> does that.\n", termsafe.Line(why))
		}
	}
	return 0
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
