package cli

import (
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
	// T6963: the CLI's own management subcommands run as they are.
	if !top && len(rest) > 0 && slices.Contains(passThroughCommands(v), rest[0]) {
		return runSubcommand(cmd, v, t, rest, stderr)
	}
	printing := !stdinTerminal()
	pv := v
	if top && (printing || len(rest) > 0) {
		pv = launchVendor()
	}
	if !printing && pv != "" {
		printing = passFlags(pv).PrintRequested(rest, nil)
	}
	if printing {
		switch {
		case resume:
			return launchUsageErr(stderr, cmd, diag.CodeUsage, "aggy -r reopens an aggy session on screen; in print mode give the CLI's own resume flag after the vendor, as aggy claude -p -r <its session>", "")
		case cont && pv != adapter.Claude && pv != adapter.Grok:
			return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("aggy -c in print mode is the CLI's own -c, which %s has no verified spelling of; run it on screen", pv), "")
		case cont:
			rest = append([]string{"--continue"}, rest...)
		}
		return runPrint(cmd, pv, t, rest, stderr)
	}
	if top {
		switch {
		case cont:
			t.Continue = true
		case resume && resumeID != "":
			id, msg := sessionArg(resumeID)
			if msg != "" {
				return launchUsageErr(stderr, cmd, diag.CodeUsage, msg, "")
			}
			t.ParentID = id
		case resume:
			t.Continue = true
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
	p, err := passFlags(v).ParseWith(rest, adapter.ParseOpts{NoPolicy: t.NoPolicy})
	if err != nil {
		return launchUsageErr(stderr, cmd, flagCode(err), err.Error(), "")
	}
	if msg := promptRefusal(v, p, p.Continue || p.Resume); msg != "" {
		return launchUsageErr(stderr, cmd, diag.CodeFlagClash, msg, vendorUsage)
	}
	switch {
	case p.Continue && p.Resume:
		return launchUsageErr(stderr, cmd, diag.CodeUsage, "use --continue or --resume, not both", vendorUsage)
	case p.Continue:
		t.Continue = true
	case p.Resume && p.ResumeID != "":
		id, msg := sessionArg(p.ResumeID)
		if msg != "" {
			return launchUsageErr(stderr, cmd, diag.CodeUsage, msg, "")
		}
		t.ParentID = id
	case p.Resume:
		t.Continue = true
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

// stdinTerminal reports whether aggy's stdin is a terminal; a test sets it.
var stdinTerminal = func() bool { return isTerminal(os.Stdin) }

// flagCode is the reason code of a PassFlags refusal.
func flagCode(err error) diag.Code {
	switch {
	case errors.Is(err, adapter.ErrNotFlag):
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
// them. values are the value flags the CLI's help lists. resuming says the
// launch resumes a session.
func parseLaunchFlags(tp *target, v adapter.Vendor, values []string, resuming bool) (diag.Code, error) {
	p, err := passFlags(v).ParseWith(tp.Flags, adapter.ParseOpts{NoPolicy: tp.NoPolicy, Values: values})
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
func runPrint(cmd string, v adapter.Vendor, t target, words []string, stderr io.Writer) int {
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
	p, err := passFlags(v).ParseWith(words, adapter.ParseOpts{NoPolicy: t.NoPolicy, Print: true})
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
	if _, err := allowedIn(cfg, cwd, v); err != nil {
		return fail(launchCode(err, diag.CodeConfig), err)
	}
	path, err := probe.LookPath(vendorBin(v))
	if err != nil {
		return fail(diag.CodeCLIMissing, err)
	}
	caps, _, err := probeCaps(ctx, v, path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: probing %s: %s; starting it without the flags help must list\n", name, v, termsafe.Line(err.Error()))
	}
	req := adapter.InteractiveRequest{Cwd: cwd, Extra: p.Extra, Print: true}
	model := p.Model
	if model == "" && v != adapter.Codex {
		// Codex's model flag is not placed for codex exec; its own -m is.
		req.Model = cfg.Vendors.For(v).Model
		model = req.Model
	}
	req.Env = childEnv(string(v), model)
	var warns []string
	if len(t.Tools) > 0 {
		warns = append(warns, "--tool is for an on-screen session, so it is ignored here")
	}
	if len(p.Loosens) > 0 {
		warns = append(warns, "your flags "+strings.Join(p.Loosens, ", ")+" loosen approvals, the sandbox, or hook trust for this run; --no-policy lets them through")
	}
	warns = append(warns, p.Notes...)
	if t.NoPolicy {
		warns = append(warns, "running without the safety policy (--no-policy)")
	} else {
		pw, err := addPolicy(v, caps, cfg.Policy.Required, &req)
		if err != nil {
			return fail(diag.CodePolicy, err)
		}
		warns = append(warns, pw...)
	}
	rules := loadPermissions(ctx, cwd, cfg)
	if rules.FileErr != nil {
		return fail(diag.CodePermBadFile, rules.FileErr)
	}
	warns = append(warns, addPermissions(v, caps, rules, &req)...)
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
	for _, w := range append(warns, mw...) {
		fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(w))
	}
	c, err := screenAdapter(v, caps).Interactive(req)
	if err != nil {
		return fail(diag.CodeUnsupported, err)
	}
	c.Path = path
	code, err := execrun.RunDirect(c)
	if err != nil {
		return fail(diag.CodeLaunchFailed, err)
	}
	r := rec
	r.Event, r.Exit = diag.EvDirectRan, diag.Int(code)
	r.Attrs = map[string]any{"mode": "print", "policy": !t.NoPolicy, "cli_words": len(p.Extra)}
	dlog.Log(ctx, r)
	return code
}

// passThroughCommands are v's management subcommands that aggy runs as they
// are (T6963). Empty until then.
func passThroughCommands(adapter.Vendor) []string { return nil }

// runSubcommand runs a vendor subcommand as it is (T6963).
func runSubcommand(cmd string, _ adapter.Vendor, _ target, rest []string, stderr io.Writer) int {
	return launchUsageErr(stderr, cmd, diag.CodeUsage, fmt.Sprintf("%q is a subcommand", rest[0]), "")
}

// commandNames are aggy's own commands, which a bare first word names before
// it can be a prompt.
var commandNames = []string{"accept", "attribution", "borrow", "brief", "check", "config", "dev", "diag", "doctor", "gc", "help",
	"hook", "hook-tool", "hooks", "image", "inbox", "log", "mcp", "memory", "new", "permissions", "policy", "reject", "resume",
	"run", "session", "sessions", "setup", "skill", "stats", "switch", "tool", "trust", "unwire", "usage", "version", "video"}

// nearCommand is the aggy command word is one edit from, or "": a single
// lower-case word that close is more likely a mistyped command than a
// prompt, as claude udpate says "Did you mean claude update?".
func nearCommand(word string) string {
	if len(word) < 4 || strings.Trim(word, "abcdefghijklmnopqrstuvwxyz-") != "" {
		return ""
	}
	for _, c := range commandNames {
		if editDistance(word, c) == 1 {
			return c
		}
	}
	return ""
}

// editDistance is the Levenshtein distance of a and b, in bytes.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
