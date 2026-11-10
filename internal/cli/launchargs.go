package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/adapter/opencode"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// runLaunch is every launch that takes a CLI's own words, as the CLI takes
// them at launch (T6961): aggy <vendor> [flags] [prompt], and, with v empty,
// aggy [flags] [prompt] on the default vendor, where -c and -r are aggy's own
// --continue and --resume. aggy's own flags (--direct, --no-policy, --tool)
// may stand anywhere before a "--".
//
// A print flag (claude -p), a print subcommand (codex exec), or a stdin that
// is no terminal runs the CLI with no supervisor and records nothing
// (runPrint). Anything else is an on-screen session: its words go to the CLI
// in order, a prompt included, after aggy's own launch flags, and are sorted
// against the vendor's PassFlags table once the launch knows the vendor
// (parseLaunchFlags). With a vendor named, they are also sorted here, so a
// refused flag is refused before anything runs.
func runLaunch(v adapter.Vendor, args []string, stdout, stderr io.Writer) int {
	cmd := string(v)
	top := v == ""
	// T6963: the CLI's own management subcommands run as they are, every word
	// after the subcommand the CLI's, aggy's flag names included.
	if !top && len(args) > 0 && slices.Contains(passThroughCommands(v), args[0]) {
		return runSubcommand(cmd, v, nil, args, stderr)
	}
	t := target{Vendor: v}
	var rest, own []string
	var cont, resume bool
	var resumeID string
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case a == "--direct":
			t.Direct, own = true, append(own, a)
		case a == "--no-policy":
			t.NoPolicy, own = true, append(own, a)
		case flag == "--tool":
			if !hasVal {
				if i+1 == len(args) {
					return launchUsageErr(stderr, cmd, diag.CodeUsage, "--tool needs a name", vendorUsage)
				}
				i++
				val = args[i]
			}
			if !tooldef.ValidName(val) {
				return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("%q is not a tool name", val), "")
			}
			t.Tools, own = append(t.Tools, val), append(own, "--tool="+val)
		case top && (a == "-c" || a == "--continue"):
			cont = true
		case top && (flag == "-r" || flag == "--resume"):
			resume = true
			if !hasVal && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				val = args[i]
			}
			resumeID = val
		default:
			rest = append(rest, a)
		}
	}
	// aggy --no-policy claude ...: the vendor named after aggy's own flags.
	if top && !cont && !resume && len(rest) > 0 {
		if nv, err := adapter.ParseVendor(rest[0]); err == nil {
			return runLaunch(nv, append(own, rest[1:]...), stdout, stderr)
		}
	}
	// aggy --direct names its vendor, as aggy run --direct does.
	if top && t.Direct && !cont && !resume {
		return runRun(args, stdout, stderr)
	}
	if cont && resume {
		return launchUsageErr(stderr, cmd, diag.CodeUsage, "use --continue or --resume, not both", continueUsage)
	}
	// aggy -r [id] [--brief [--show]] is aggy resume's, as before.
	if resume && !t.Direct && len(t.Tools) == 0 && len(rest) > 0 && !slices.ContainsFunc(rest, func(w string) bool { return w != "--brief" && w != "--show" }) {
		ra := slices.Clone(rest)
		if t.NoPolicy {
			ra = append(ra, "--no-policy")
		}
		if resumeID != "" {
			ra = append(ra, resumeID)
		}
		return runResumeFlag(ra, stdout, stderr)
	}
	// T6963: a management subcommand after one of aggy's own flags (the one
	// first on the line ran above, before any word was sorted).
	if !top && len(rest) > 0 && slices.Contains(passThroughCommands(v), rest[0]) {
		return runSubcommand(cmd, v, own, rest, stderr)
	}
	mode := adapter.RunScreen
	if !stdinTerminal() {
		mode = adapter.RunPrint
		if len(rest) == 0 && !cont && !resume {
			// aggy </dev/null: no session to show and no prompt to answer
			// (PR #124 review).
			return launchUsageErr(stderr, cmd, diag.CodeNoTerminal, "needs a terminal or a prompt: stdin is not a terminal and no prompt was given", "")
		}
	}
	pv := v
	if top && (mode != adapter.RunScreen || len(rest) > 0) {
		pv = launchVendor()
	}
	if top && len(rest) > 0 {
		// aggy -p "x" is Claude's and Grok's print form; on Codex -p is
		// --profile, and OpenCode takes none (PR #124 review).
		if msg := printFormRefusal(pv, rest); msg != "" {
			return launchUsageErr(stderr, cmd, diag.CodeUsage, msg, "")
		}
	}
	var help adapter.ParseOpts // the CLI's value flags and switches
	if len(rest) > 0 {
		if m := passFlags(pv).Mode(rest); m > mode {
			mode = m
		}
		// The CLI's help says which flags take a value (T6962). codex exec's
		// flags are its own, not the top-level help's, so none is assumed
		// there: a word after one of them is taken for the prompt, and the run
		// still gets aggy's policy.
		if !slices.Contains(passFlags(pv).PrintCommands, rest[0]) {
			help = cliHelpFlags(pv)
		}
	}
	if mode == adapter.RunBare {
		if cont || resume {
			return launchUsageErr(stderr, cmd, diag.CodeUsage, "-c and -r reopen a session; this flag starts none", "")
		}
		return runBare(cmd, pv, t, rest, stderr)
	}
	if mode == adapter.RunPrint {
		switch {
		case resume:
			return launchUsageErr(stderr, cmd, diag.CodeUsage, "aggy -r reopens an aggy session on screen; in print mode give the CLI's own resume flag after the vendor, as aggy claude -p -r <its session>", "")
		case cont && pv != adapter.Claude && pv != adapter.Grok:
			return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("aggy -c in print mode is the CLI's own -c, which %s has no verified spelling of; run it on screen", pv), "")
		case cont:
			rest = append([]string{"--continue"}, rest...)
		}
		return runPrint(cmd, pv, t, rest, help, stderr)
	}
	if top {
		switch {
		case cont:
			t.Continue = true
		case resume && resumeID != "":
			if code, err := resolveResume(&t, "", resumeID); err != nil {
				return resumeRefused(stderr, cmd, code, err)
			}
		case resume:
			picked, code, err := pickSession(&t, "", stderr)
			if err != nil {
				return resumeRefused(stderr, cmd, code, err)
			}
			if !picked {
				return 0
			}
		default:
			t.New = true
		}
		// The default vendor, or the vendor of the session -c or -r finds, is
		// known once the launch has planned it.
		if len(rest) > 0 {
			t.Flags = rest
		}
		return launch("aggy", t, stderr)
	}
	p, err := passFlags(v).ParseWith(rest, adapter.ParseOpts{NoPolicy: t.NoPolicy, Values: help.Values, Switches: help.Switches})
	if err != nil {
		return launchUsageErr(stderr, cmd, flagCode(err), err.Error(), "")
	}
	if msg := promptRefusal(v, p, p.Continue || p.Resume); msg != "" {
		return launchUsageErr(stderr, cmd, diag.CodeFlagClash, msg, vendorUsage)
	}
	// A --settings layer that defeats the policy is refused here too, before
	// anything runs; the launch reads it again in the session's directory.
	if cwd, err := os.Getwd(); err == nil && v == adapter.Claude {
		if _, _, _, _, err := claudeSettings(p.Extra, cwd, t.NoPolicy); errors.Is(err, adapter.ErrLoosens) || errors.Is(err, claude.ErrSettingsCredential) {
			return launchUsageErr(stderr, cmd, settingsCode(err), err.Error(), "")
		}
	}
	switch {
	case p.Continue && p.Resume:
		return launchUsageErr(stderr, cmd, diag.CodeUsage, "use --continue or --resume, not both", vendorUsage)
	case p.Continue:
		t.Continue = true
	case p.Resume && p.ResumeID != "":
		if code, err := resolveResume(&t, v, p.ResumeID); err != nil {
			return resumeRefused(stderr, cmd, code, err)
		}
	case p.Resume:
		picked, code, err := pickSession(&t, v, stderr)
		if err != nil {
			return resumeRefused(stderr, cmd, code, err)
		}
		if !picked {
			return 0
		}
	default:
		t.New = true
	}
	t.Model = p.Model
	t.Flags = rest
	if t.Flags == nil {
		t.Flags = []string{}
	}
	return launch("aggy "+cmd, t, stderr)
}

// resumeRefused refuses a -r that finds no session (exit 1, as a launch that
// finds none) or is given a value aggy will not pass (exit 2, a usage error).
func resumeRefused(stderr io.Writer, cmd string, code diag.Code, err error) int {
	if code == diag.CodeUsage {
		return launchUsageErr(stderr, cmd, code, err.Error(), "")
	}
	refuse(context.Background(), stderr, cmd, diag.EvLaunchRefused, code, err.Error(), diag.Rec{})
	return 1
}

// stdinTerminal reports whether aggy's stdin is a terminal; a test sets it.
var stdinTerminal = func() bool { return isTerminal(os.Stdin) }

// flagCode is the reason code of a PassFlags refusal.
func flagCode(err error) diag.Code {
	switch {
	case errors.Is(err, adapter.ErrNotFlag), errors.Is(err, adapter.ErrCommandAfterDashes),
		errors.Is(err, adapter.ErrDashesValue), errors.Is(err, adapter.ErrOwnedAfterDashes):
		return diag.CodeUsage
	case errors.Is(err, adapter.ErrLoosens):
		return diag.CodeFlagLoosens
	}
	return diag.CodeFlagClash
}

// promptRefusal says why the words p sorted cannot go to v's CLI on screen, or
// "". OpenCode takes none of its own words yet. A prompt with a resume is in
// Claude's reference (claude -r "<session>" "query"); for Codex and Grok it is
// unverified, so it is typed once the session is back.
func promptRefusal(v adapter.Vendor, p adapter.ParsedFlags, resuming bool) string {
	switch {
	case v == adapter.OpenCode && len(p.Prompt) > 0:
		return "aggy passes no prompt to OpenCode yet; type it once the session starts"
	case v == adapter.OpenCode && len(p.Extra) > 0:
		return fmt.Sprintf("aggy passes none of OpenCode's own flags yet, so not %q", p.Extra[0])
	case len(p.Prompt) > 0 && resuming && v != adapter.Claude:
		return fmt.Sprintf("a prompt with a resumed %s session is unverified; type it once the session is back", v)
	}
	return ""
}

// parseLaunchFlags sorts tp.Flags, the CLI's words, against v's table, now
// that the launch knows v (the default vendor, or the vendor of the session
// -c or -r found), and sets the target's model, CLI words, and notes from
// them. help holds the value flags and switches the CLI's help lists. resuming says the
// launch resumes a session.
func parseLaunchFlags(tp *target, v adapter.Vendor, help adapter.ParseOpts, resuming bool) (diag.Code, error) {
	p, err := passFlags(v).ParseWith(tp.Flags, adapter.ParseOpts{NoPolicy: tp.NoPolicy, Values: help.Values, Switches: help.Switches})
	if err != nil {
		return flagCode(err), err
	}
	if tp.Vendor == "" && (p.Continue || p.Resume) {
		return diag.CodeUsage, errors.New("write aggy's -c or -r before the CLI's flags, once")
	}
	if msg := promptRefusal(v, p, resuming); msg != "" {
		return diag.CodeFlagClash, errors.New(msg)
	}
	if p.Model != "" {
		tp.Model = p.Model
	}
	tp.Extra, tp.SafeExtra, tp.Loosens, tp.FlagNotes, tp.Dirs = p.Extra, p.Safe, p.Loosens, p.Notes, p.Dirs
	tp.Prompt = len(p.Prompt) > 0
	tp.Flags = nil
	return diag.Code{}, nil
}

// launchVendor is the vendor a launch with none named starts here: the rule
// plan follows (defaultVendor), read without writing anything. Any error
// leaves it to the launch, which reports it; claude stands in meanwhile.
func launchVendor() adapter.Vendor {
	cfg, err := config.LoadDefault()
	if err != nil {
		return adapter.Claude
	}
	cwd, err := os.Getwd()
	if err != nil {
		return adapter.Claude
	}
	eff, err := repoEffective(cfg, cwd)
	if err != nil {
		return adapter.Claude
	}
	ctx := context.Background()
	var s *store.Store
	if path, err := store.DefaultPath(); err == nil {
		if s, err = store.OpenReadOnly(ctx, path); err == nil {
			defer func() { _ = s.Close() }()
		} else {
			s = nil
		}
	}
	return defaultVendor(ctx, s, eff.Config, eff.Allowed)
}

// screenAdapter is v's adapter for a launch, with the probe's capabilities.
func screenAdapter(v adapter.Vendor, caps []string) adapter.Adapter {
	switch v {
	case adapter.Codex:
		return codex.Adapter{Caps: caps}
	case adapter.Grok:
		return grok.New()
	case adapter.OpenCode:
		return opencode.New()
	}
	return claude.Adapter{Caps: caps}
}

// runPrint runs v's CLI directly on the person's terminal (execrun.RunDirect):
// print mode, a print subcommand, or a stdin that is no terminal. There is no
// session to supervise, switch, or record, so nothing goes in the index, and
// stdin, stdout, and the exit code are the CLI's own. It still gets what aggy
// adds at an on-screen launch: the safety policy (refused, as on screen, when it
// cannot be applied, unless --no-policy or [policy] required = false), the
// shared rules, the shared memory, and the configured model when none is
// given. words are the CLI's own, passed in order; aggy takes over none of
// them (ParseOpts.Print), and refuses or gates the same flags as on screen,
// except the ones that clash only with the session aggy records.
func runPrint(cmd string, v adapter.Vendor, t target, words []string, help adapter.ParseOpts, stderr io.Writer) int {
	ctx := context.Background()
	name := strings.TrimSpace("aggy " + cmd)
	rec := diag.Rec{Vendor: string(v)}
	fail := func(code diag.Code, err error) int {
		refuse(ctx, stderr, cmd, diag.EvLaunchRefused, code, err.Error(), rec)
		return 1
	}
	if v == adapter.OpenCode {
		return launchUsageErr(stderr, cmd, diag.CodeUnsupported, "aggy runs OpenCode on screen only: print mode and a stdin that is no terminal are not wired for it; run opencode itself", "")
	}
	// Sorted first, so a refused flag is refused before anything is looked up.
	p, err := passFlags(v).ParseWith(words, adapter.ParseOpts{NoPolicy: t.NoPolicy, Print: true, Values: help.Values, Switches: help.Switches})
	if err != nil {
		return launchUsageErr(stderr, cmd, flagCode(err), err.Error(), "")
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return fail(diag.CodeConfig, err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	// The same gate as on screen (PR #124 review): delegation, allowed vendors,
	// folder flags, --settings, who asked for --no-policy, and the shared rules.
	gt := target{Vendor: v, NoPolicy: t.NoPolicy, Extra: p.Extra, SafeExtra: p.Safe, Loosens: p.Loosens, Dirs: p.Dirs}
	g, code, err := gateLaunch(ctx, &gt, v, cwd, cfg, true)
	if err != nil {
		return launchUsageErr(stderr, cmd, code, err.Error(), "")
	}
	p.Loosens = gt.Loosens
	req := adapter.InteractiveRequest{Cwd: cwd, Extra: g.extra, UserSettings: g.layer, Print: true}
	path, err := probe.LookPath(vendorBin(v))
	if err != nil {
		return fail(diag.CodeCLIMissing, err)
	}
	// Checked, so the probe may record (probeCLI).
	probed, err := probeCLI(ctx, v, path, true)
	if err != nil {
		fmt.Fprintf(stderr, "%s: probing %s: %s; starting it without the flags help must list\n", name, v, termsafe.Line(err.Error()))
	}
	caps := probed.Capabilities
	var warns []string
	model := p.Model
	if model == "" && v != adapter.Codex {
		// Codex's model flag is not placed for codex exec; its own -m is.
		req.Model = cfg.Vendors.For(v).Model
		model = req.Model
	}
	req.Env = childEnv(string(v), model)
	if len(t.Tools) > 0 {
		warns = append(warns, "--tool is for an on-screen session, so it is ignored here")
	}
	if len(p.Loosens) > 0 {
		warns = append(warns, "your flags "+strings.Join(p.Loosens, ", ")+" loosen approvals, the sandbox, or hook trust for this run; --no-policy lets them through")
	}
	warns = append(warns, p.Notes...)
	rules := loadPermissions(ctx, cwd, cfg)
	if rules.FileErr != nil {
		return fail(diag.CodePermBadFile, rules.FileErr)
	}
	if t.NoPolicy {
		warns = append(warns, "running without the safety policy (--no-policy)")
	} else {
		// Every rule a deny (PR #125 integration): no one may be there to
		// answer an ask, and a --permission-prompt-tool could answer it.
		pw, err := addPolicy(v, caps, cfg.Policy.Required, policyPlan(planHeadless, cwd, rules), &req)
		if err != nil {
			return fail(diag.CodePolicy, err)
		}
		warns = append(warns, pw...)
	}
	warns = append(warns, addPermissions(v, caps, rules, &req)...)
	if v == adapter.Claude {
		warns = append(warns, settingsNotes(req)...)
	}
	stateDir, err := home.Dir()
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	runDir := filepath.Join(stateDir, "run")
	if err := supervisor.PrivateDir(runDir); err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	// The memory folder is named for a parent no supervisor holds, so a
	// sweep may take it a minute on: the CLI reads its files at start.
	id, err := newUUID()
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	memDir, err := memoryDir(runDir, id)
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	cleanup, mw := addMemory(v, caps, cwd, memDir, &req)
	defer cleanup()
	defer func() { _ = safefs.RemoveAll(memDir) }()
	if err := joinAppendFiles(&req, cwd); err != nil {
		return fail(diag.CodeUsage, err)
	}
	if err := writeSettingsFile(&req, g, memDir); err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	for _, w := range append(warns, mw...) {
		fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(w))
	}
	c, err := screenAdapter(v, caps).Interactive(req)
	if err != nil {
		return fail(diag.CodeUnsupported, err)
	}
	c.Path = path
	exit, err := execrun.RunDirect(c)
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	r := rec
	r.Event, r.Exit = diag.EvDirectRan, diag.Int(exit)
	r.Attrs = map[string]any{"mode": "print", "policy": !t.NoPolicy, "cli_words": len(p.Extra)}
	dlog.Log(ctx, r)
	return exit
}

// runBare runs v's CLI directly with words as given and nothing added: a Bare
// flag (--version) that is the only word (PassFlags.Mode), which starts no
// session. It still goes through gateLaunch. Nothing is recorded.
func runBare(cmd string, v adapter.Vendor, t target, words []string, stderr io.Writer) int {
	if passFlags(v).Mode(words) != adapter.RunBare {
		// Mode chose it; this keeps runBare from running anything else.
		return launchUsageErr(stderr, cmd, diag.CodeUsage, "only a version flag, alone, runs as given", "")
	}
	if t.NoPolicy || len(t.Tools) > 0 {
		return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("%s alone runs as given, with none of aggy's flags", words[0]), "")
	}
	return runAsGiven(cmd, v, words, "bare", stderr)
}

// runAsGiven runs v's CLI with words, on the person's terminal, with their own
// environment and nothing of aggy's added (execrun.RunDirect), and returns its
// exit code. aggy reads nothing the CLI writes. gateLaunch runs first, with no
// words to sort: a borrow child, a vendor the repo leaves out, and shared rules
// that cannot be read are refused here too.
func runAsGiven(cmd string, v adapter.Vendor, words []string, mode string, stderr io.Writer) int {
	ctx := context.Background()
	rec := diag.Rec{Vendor: string(v)}
	fail := func(code diag.Code, err error) int {
		refuse(ctx, stderr, cmd, diag.EvLaunchRefused, code, err.Error(), rec)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		return fail(diag.CodeConfig, err)
	}
	if _, code, err := gateLaunch(ctx, &target{Vendor: v}, v, cwd, cfg, true); err != nil {
		return launchUsageErr(stderr, cmd, code, err.Error(), "")
	}
	path, err := probe.LookPath(vendorBin(v))
	if err != nil {
		return fail(diag.CodeCLIMissing, err)
	}
	c := adapter.Command{Path: path, Args: slices.Clone(words), Dir: cwd, Stdin: adapter.StdinTerminal}
	code, err := execrun.RunDirect(c)
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	rec.Event, rec.Exit = diag.EvDirectRan, diag.Int(code)
	rec.Attrs = map[string]any{"mode": mode, "policy": false, "cli_words": len(words)}
	dlog.Log(ctx, rec)
	return code
}

// cliHelpFlags are the flags v's CLI's help shows with a value and with none,
// from its probe, or none when the CLI is not on PATH or the probe fails (then
// no "--" after a flag is taken as the end of options). It runs before the
// launch's checks, so it records nothing (probeCLI's record is false).
func cliHelpFlags(v adapter.Vendor) adapter.ParseOpts {
	path, err := probe.LookPath(vendorBin(v))
	if err != nil {
		return adapter.ParseOpts{}
	}
	pr, err := probeCLI(context.Background(), v, path, false)
	if err != nil {
		return adapter.ParseOpts{}
	}
	return adapter.ParseOpts{Values: pr.ValueFlags, Switches: pr.Switches}
}

// printFormRefusal says why aggy -p, with no vendor named, cannot go to the
// default vendor pv, or "": -p is print mode on Claude and Grok, but --profile
// on Codex, and OpenCode takes none (PR #124 review). It names the vendor's own
// print form.
func printFormRefusal(pv adapter.Vendor, words []string) string {
	pf := passFlags(pv)
	for _, w := range words {
		if w == "--" {
			break
		}
		name, _, _ := strings.Cut(w, "=")
		if (name == "-p" || name == "--print") && !slices.Contains(pf.Print, name) {
			switch {
			case len(pf.PrintCommands) > 0:
				return fmt.Sprintf("the default vendor here is %s, whose print form is %s %s, not %s; run aggy %s %s", pv, pv, pf.PrintCommands[0], name, pv, pf.PrintCommands[0])
			default:
				return fmt.Sprintf("the default vendor here is %s, which has no print mode aggy runs; name another vendor, as aggy claude -p", pv)
			}
		}
	}
	return ""
}

// settingsCode is the reason code of a claudeSettings refusal.
func settingsCode(err error) diag.Code {
	if errors.Is(err, adapter.ErrLoosens) {
		return diag.CodeFlagLoosens
	}
	return diag.CodeUsage
}

// claudeSettings takes the person's --settings out of extra and reads them, a
// path against cwd through safefs, up to Claude's own 2 MiB (T6962): their
// layers merged in order, and extra without them, for Claude's adapter to merge
// under aggy's own layer. A layer that would get past aggy's policy
// (claude.CheckSettings) is refused, unless noPolicy, which reports it; one
// that sets a credential is refused either way. fromFile says a value was a
// file, whose content must stay off argv (writeSettingsFile).
func claudeSettings(extra []string, cwd string, noPolicy bool) (layer map[string]any, rest []string, loosens, fromFile bool, err error) {
	vals, rest := adapter.TakeFlag(extra, "--settings")
	for _, val := range vals {
		b := []byte(val)
		if !claude.InlineSettings(val) {
			fromFile = true
			p := val
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			if b, err = safefs.ReadFileLimit(p, claude.MaxSettings); err != nil {
				return nil, nil, false, false, fmt.Errorf("--settings %s: %w", val, err)
			}
		}
		m, err := claude.ParseSettings(b)
		if err != nil {
			return nil, nil, false, false, err
		}
		if layer == nil {
			layer = m
		} else {
			layer = claude.MergeSettings(layer, m)
		}
	}
	if err := claude.CheckSettings(layer); err != nil {
		if !noPolicy || !errors.Is(err, adapter.ErrLoosens) {
			return nil, nil, false, false, err
		}
		loosens = true
	}
	return layer, rest, loosens, fromFile, nil
}

// joinAppendFiles puts the text of the person's --append-system-prompt-file
// flags into the shared memory's file, ahead of the memory, and takes the
// flags out of req.Extra, when aggy passes that file too (T6962): neither is
// dropped, and whether Claude takes the flag twice is unrecorded. A path is read
// against cwd, through safefs.
func joinAppendFiles(req *adapter.InteractiveRequest, cwd string) error {
	if req.MemoryFile == "" {
		return nil
	}
	vals, rest := adapter.TakeFlag(req.Extra, "--append-system-prompt-file")
	if len(vals) == 0 {
		return nil
	}
	var parts [][]byte
	for _, val := range vals {
		p := val
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		b, err := safefs.ReadFileLimit(p, maxAppendFile)
		if err != nil {
			return fmt.Errorf("--append-system-prompt-file %s: %w", val, err)
		}
		parts = append(parts, b)
	}
	mem, err := safefs.ReadFile(req.MemoryFile)
	if err != nil {
		return err
	}
	parts = append(parts, mem)
	if err := safefs.WriteFile(req.MemoryFile, bytes.Join(parts, []byte("\n\n")), 0o600); err != nil {
		return err
	}
	req.Extra = rest
	return nil
}

// maxAppendFile caps a file --append-system-prompt-file names.
const maxAppendFile = 1 << 20

// passThroughCommands are v's management subcommands that aggy runs as they
// are (T6963): adapter.Manage, with its sources there.
func passThroughCommands(v adapter.Vendor) []string { return adapter.Manage[v].Commands }

// runSubcommand runs a management subcommand of v's CLI (T6963): args as
// typed, the subcommand first, on the person's terminal with their own
// environment, and no supervisor, hooks, policy, or flag of aggy's
// (runAsGiven). A sign-in writes what it writes; aggy reads none of it.
// aggy's own flags mean nothing there, so a subcommand after one is refused.
//
// The PR #124 review: a subcommand whose second word runs an agent after all
// (ManageRefused, as claude mcp serve) is refused, and so is any flag the
// vendor's table gates anywhere in the words (PassFlags.Gated), since aggy adds
// no policy here for --no-policy to let it through. gateLaunch runs too.
func runSubcommand(cmd string, v adapter.Vendor, own, args []string, stderr io.Writer) int {
	if len(own) > 0 {
		return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("%s %s runs as typed, with none of aggy's flags; drop %s", v, args[0], own[0]), "")
	}
	sub, unsure := secondWord(args[1:])
	if unsure {
		return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("%s %s: a -- or a flag before the subcommand's own word is refused, since aggy cannot tell what the CLI runs then; put %s's own word first", v, args[0], args[0]), "")
	}
	if slices.ContainsFunc(manageRefused(v)[args[0]], func(r string) bool { return strings.EqualFold(r, sub) }) {
		return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("%s %s %s serves the CLI's own tools, which aggy would run with no policy; run %s itself for it", v, args[0], sub, v), "")
	}
	if err := passFlags(v).Gated(args[1:], "--settings"); err != nil {
		return launchUsageErr(stderr, cmd, diag.CodeFlagClash, fmt.Sprintf("%s %s: %s; run %s itself for it", v, args[0], err, v), "")
	}
	return runAsGiven(cmd, v, args, "subcommand", stderr)
}

// secondWord is the first word of words that is neither a flag nor "--", or
// "", and whether something before it leaves open which word the CLI takes for
// the subcommand (unsure): a "--" (or, with no such word, a "--" at all), or
// any flag, which may take that word as its value and leave the next one the
// subcommand (claude mcp --x serve). The PR #124 re-reviews: stopping at "--"
// let claude mcp -- serve through, so the word is found past it, and the
// caller refuses an unsure one. A later "--" is the subcommand's own (claude
// mcp add name -- cmd args).
func secondWord(words []string) (word string, unsure bool) {
	dashes, flag := false, false
	for _, w := range words {
		switch {
		case w == "--":
			dashes = true
		case strings.HasPrefix(w, "-"):
			flag = true
		default:
			return w, dashes || flag
		}
	}
	return "", dashes
}

// manageRefused is v's refused second words (adapter.Manage).
func manageRefused(v adapter.Vendor) map[string][]string { return adapter.Manage[v].Refused }

// commandNames are aggy's own commands, which a bare first word names before
// it can be a prompt. The guard hook reads the same list.
var commandNames = hook.AggyCommands

// commandAbbrevs are short forms a person may type for a command, which are
// taken as typos of it rather than prompts.
var commandAbbrevs = map[string]string{"img": "image", "vid": "video", "perms": "permissions", "cfg": "config", "sess": "sessions"}

// nearCommand is the aggy command or vendor name word is close to, or "": a
// first word that close is more likely a mistyped command than a prompt, as
// claude udpate says "Did you mean claude update?" (PR #124 review). The
// re-review's rule: case is ignored (Doctor is a typo of doctor), and a typo
// is exactly one edit (oneEdit): one letter changed, two neighbouring letters
// swapped (dcotor), or one letter missing (sesions, claud). An extra letter is
// not taken for one, so tools, logs, and news stay prompts beside tool, log,
// and new. Both the word and the command need four letters or more; a listed
// short form (img) needs three. Only letters and "-" are looked at, so a
// prompt with a space in it is never a typo.
func nearCommand(word string) string {
	low := strings.ToLower(word)
	if len(low) < 3 || low[0] == '-' || strings.Trim(low, "abcdefghijklmnopqrstuvwxyz-") != "" {
		return ""
	}
	if c, ok := commandAbbrevs[low]; ok {
		return c
	}
	if len(low) < 4 {
		return ""
	}
	for _, c := range append(slices.Clone(commandNames), "claude", "codex", "grok", "opencode") {
		if len(c) < 4 || word == c {
			continue
		}
		if low == c || oneEdit(low, c) {
			return c
		}
	}
	return ""
}

// oneEdit reports whether word is c with exactly one letter changed, two
// neighbouring letters swapped, or one letter left out.
func oneEdit(word, c string) bool {
	switch len(word) {
	case len(c):
		var diff []int
		for i := range word {
			if word[i] != c[i] {
				diff = append(diff, i)
			}
		}
		switch len(diff) {
		case 1:
			return true
		case 2:
			i, j := diff[0], diff[1]
			return j == i+1 && word[i] == c[j] && word[j] == c[i]
		}
		return false
	case len(c) - 1:
		for i := range c {
			if c[:i]+c[i+1:] == word {
				return true
			}
		}
	}
	return false
}
