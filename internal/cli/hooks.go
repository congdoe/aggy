package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const hooksUsage = `Usage: aggy hooks install [--vendor V] [--dry-run]
       aggy hooks status

Installs the hooks that tell aggy when a vendor CLI's session starts, its
turn ends, or it hits a limit. Each hook runs this aggy binary, by its
absolute path (the one on PATH when it is the same file), as aggy hook
--vendor V <event>; outside aggy it does nothing, and once that binary is
gone it does nothing quietly. The binary and every directory above it
must belong to you or root and be writable by no one else, since every
session of each CLI runs it. Existing hooks are kept; aggy's own entry
from another path is updated. Every change goes through the manifest;
aggy unwire takes out aggy's entries, leaving the rest of the file as it
is then.

  claude   ~/.claude/settings.json: Stop, StopFailure, SessionStart, Notification
  codex    ~/.codex/hooks.json ($CODEX_HOME): Stop, SessionStart. Codex
           runs hooks only after you approve them once (start codex,
           then /hooks).
  grok     ~/.grok/hooks/aggy.json ($GROK_HOME/hooks): Stop, StopFailure,
           StopCancelled, SessionStart, Notification. Grok also runs
           Claude's settings.json hooks; aggy hook ignores those inside
           Grok, so each event counts once.

  --vendor V   install for one vendor only (default: all)
  --dry-run    show the changes and make none
`

// hookBin is this aggy binary; tests replace it.
var hookBin = os.Executable

// pathAggy finds aggy on PATH; tests replace it.
var pathAggy = func() (string, error) { return probe.LookPath("aggy") }

// checkTrusted is trustedPath; tests whose temp dirs sit under a
// world-writable /tmp replace it.
var checkTrusted = trustedPath

// hookPlan is what installing one vendor's hooks needs.
type hookPlan struct {
	vendor  adapter.Vendor
	patches []patch.Patch
	note    string // shown with the vendor's status, e.g. an approval step
}

// hookFile is one Claude-format hooks file and aggy's entries in it.
type hookFile struct {
	path    string
	entries []hook.Entry
}

func entries(v adapter.Vendor, events ...string) []hook.Entry {
	out := make([]hook.Entry, len(events))
	for i, e := range events {
		out[i] = hook.Entry{Vendor: v, Event: e}
	}
	return out
}

// hookFiles lists where each vendor's hooks go. Sources: Claude's
// ~/.claude/settings.json "hooks" (code.claude.com/docs/en/hooks.md;
// CLAUDE_CONFIG_DIR is unverified, as for skills, so not followed);
// Codex's $CODEX_HOME/hooks.json (docs/verification.md, Codex hooks:
// hook_config.rs#L12, engine/discovery.rs#L378); Grok's
// ~/.grok/hooks/*.json, Claude's format (grok-build 2bdd1d6a,
// user-guide/10-hooks.md). That $GROK_HOME moves the hooks folder, as it
// moves skills and sessions, is unverified.
func hookFiles(v adapter.Vendor) ([]hookFile, error) {
	h := os.Getenv("HOME")
	if !filepath.IsAbs(h) {
		return nil, errors.New("HOME is not an absolute path")
	}
	switch v {
	case adapter.Claude:
		return []hookFile{{filepath.Join(h, ".claude", "settings.json"),
			entries(v, "Stop", "StopFailure", "SessionStart", "Notification")}}, nil
	case adapter.Codex:
		ch, err := codexHome(h)
		if err != nil {
			return nil, err
		}
		// Codex has no failure hook (verification.md, Codex limits).
		return []hookFile{{filepath.Join(ch, "hooks.json"), entries(v, "Stop", "SessionStart")}}, nil
	case adapter.Grok:
		gh := filepath.Join(h, ".grok")
		if g := os.Getenv("GROK_HOME"); g != "" {
			if !filepath.IsAbs(g) {
				return nil, fmt.Errorf("GROK_HOME=%q is not an absolute path", g)
			}
			gh = filepath.Clean(g)
		}
		// Grok's own hooks, so StopCancelled is heard and Grok is covered
		// with [compat.claude] hooks = false; hook.Forward drops the
		// Claude ones Grok also runs.
		return []hookFile{{filepath.Join(gh, "hooks", "aggy.json"),
			entries(v, "Stop", "StopFailure", "StopCancelled", "SessionStart", "Notification")}}, nil
	}
	return nil, nil
}

// codexHome is $CODEX_HOME, or ~/.codex.
func codexHome(h string) (string, error) {
	if c := os.Getenv("CODEX_HOME"); c != "" {
		if !filepath.IsAbs(c) {
			return "", fmt.Errorf("CODEX_HOME=%q is not an absolute path", c)
		}
		return filepath.Clean(c), nil
	}
	return filepath.Join(h, ".codex"), nil
}

// codexTrustNote is shown wherever Codex's hooks are installed. Codex
// runs a hook only once its hash is trusted (verification.md, trust
// gate), which the person approves in interactive Codex. aggy has no
// documented way to read that state and never bypasses it.
//
// aggy installs no Codex notify: it would hand aggy every turn's prompts
// and reply as argv (legacy_notify.rs), readable by other users through
// the process table, for every Codex session, and Stop gives turn ends
// and the thread id.
const codexTrustNote = "Codex runs its hooks only once you approve them (start codex, then /hooks), which aggy cannot check; until then aggy does not hear Codex turn ends or learn its session id."

// planHooks returns the patches that install v's hooks for bin. It
// writes nothing.
func planHooks(v adapter.Vendor, bin string) (hookPlan, error) {
	p := hookPlan{vendor: v}
	files, err := hookFiles(v)
	if err != nil {
		return p, err
	}
	for _, f := range files {
		src, h, err := readMaybe(f.path)
		if err != nil {
			return p, err
		}
		missing, err := hook.Missing(src, bin, f.entries)
		if err != nil {
			return p, fmt.Errorf("%s: %w", f.path, err)
		}
		if missing > 0 {
			p.patches = append(p.patches, patch.Patch{File: f.path, ExpectedHash: h,
				Transform: hook.Merge(bin, f.entries), Reverse: manifest.ReverseAggyHooks})
		}
	}
	if v == adapter.Codex {
		p.note = codexTrustNote
	}
	return p, nil
}

// aggyBin returns the path hooks should call: aggy on PATH when it is
// this same binary (Homebrew's stable bin/aggy link rather than the
// versioned Cellar path os.Executable can give), else this binary's
// path. It refuses a go run build, a path a hook command cannot carry
// unquoted, and one anybody but the user or root could change.
func aggyBin() (string, error) {
	exe, err := hookBin()
	if err != nil {
		return "", err
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return "", errors.New("aggy is running from go run's temporary build; install it (make build) and run that binary")
	}
	bin := exe
	if p, err := pathAggy(); err == nil && p != exe && filepath.IsAbs(p) && sameFile(p, exe) {
		bin = filepath.Clean(p)
	}
	if err := hook.CheckBin(bin); err != nil {
		return "", err
	}
	return bin, checkTrusted(bin)
}

func sameFile(a, b string) bool {
	fa, err := safefs.Stat(a)
	if err != nil {
		return false
	}
	fb, err := safefs.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

func runHooks(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return cmdUsage(stderr, "hooks", "name a subcommand", hooksUsage)
	}
	if args[0] == "help" {
		fmt.Fprint(stdout, hooksUsage)
		return 0
	}
	op := args[0]
	fl := flag.NewFlagSet("hooks", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	vendor := fl.String("vendor", "", "")
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 {
		return cmdUsage(stderr, "hooks", "bad arguments", hooksUsage)
	}
	vendors := adapter.Vendors()
	if *vendor != "" {
		v, err := adapter.ParseVendor(*vendor)
		if err != nil {
			return cmdUsage(stderr, "hooks", err.Error(), "")
		}
		vendors = []adapter.Vendor{v}
	}
	if op != "status" && op != "install" {
		return cmdUsage(stderr, "hooks", fmt.Sprintf("unknown subcommand %q", op), hooksUsage)
	}
	bin, err := aggyBin()
	if err != nil {
		return cmdErrDry(stderr, "hooks", diag.CodeWireFailed, err, *dryRun)
	}
	switch op {
	case "status":
		if *vendor != "" || *dryRun {
			return cmdUsage(stderr, "hooks status", "takes no flags", "")
		}
		code := 0
		for _, v := range adapter.Vendors() {
			st, note := hookStatus(v, bin)
			if st == hookError {
				code = 1
			}
			fmt.Fprintf(stdout, "%-7s %s\n", v, termsafe.Line(st+note))
		}
		return code
	}
	var ps []patch.Patch
	var notes []string
	for _, v := range vendors {
		p, err := planHooks(v, bin)
		if err != nil {
			return cmdErrDry(stderr, "hooks install", diag.CodeWireFailed, fmt.Errorf("%s: %w", v, err), *dryRun)
		}
		ps = append(ps, p.patches...)
		if p.note != "" {
			notes = append(notes, string(v)+": "+p.note)
		}
	}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return cmdErrDry(stderr, "hooks install", diag.CodeWireFailed, err, true)
		}
		return 0
	}
	if len(ps) == 0 {
		fmt.Fprintln(stdout, "Nothing to change: aggy's hooks are installed.")
		printNotes(stdout, notes)
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return cmdErr(stderr, "hooks", diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return cmdErr(stderr, "hooks", diag.CodeWireFailed, err)
	}
	runID := manifest.NewRunID()
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			fmt.Fprintf(stdout, "changed %s\n", termsafe.Field(c.File))
		}
	}
	if err != nil {
		return cmdErr(stderr, "hooks install", diag.CodeWireFailed, err)
	}
	printNotes(stdout, notes)
	fmt.Fprintf(stdout, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return 0
}

func printNotes(w io.Writer, notes []string) {
	for _, n := range notes {
		fmt.Fprintln(w, termsafe.Line(n))
	}
}

// Hook states, as aggy hooks status and aggy doctor show them.
const (
	hookInstalled = "installed"
	hookMissing   = "not installed (aggy hooks install)"
	hookPartial   = "partly installed (aggy hooks install adds the rest)"
	hookStale     = "installed for another aggy binary (aggy hooks install updates them)"
	hookError     = "cannot be installed as is"
)

// hookStatus says whether v's hooks are in place for bin, with a note:
// the reason when they cannot be installed, or a step left to the user.
// It writes nothing.
func hookStatus(v adapter.Vendor, bin string) (state, note string) {
	p, err := planHooks(v, bin)
	switch {
	case err != nil:
		return hookError, ": " + err.Error()
	}
	if p.note != "" {
		note = ": " + p.note
	}
	pr, err := surveyHooks(v, bin)
	if err != nil {
		return hookError, ": " + err.Error()
	}
	switch {
	case len(pr.Exact) == 0 && len(pr.Stale) == 0:
		return hookMissing, note
	case len(pr.Absent) > 0:
		return hookPartial, note
	case len(pr.Stale) > 0:
		return hookStale, note
	}
	return hookInstalled, note
}

// surveyHooks says how v's hooks stand against bin, across its hook files. It
// writes nothing.
func surveyHooks(v adapter.Vendor, bin string) (hook.Presence, error) {
	var all hook.Presence
	files, err := hookFiles(v)
	if err != nil {
		return all, err
	}
	for _, f := range files {
		src, _, err := readMaybe(f.path)
		if err != nil {
			return all, err
		}
		pr, err := hook.Survey(src, bin, f.entries)
		if err != nil {
			return all, fmt.Errorf("%s: %w", f.path, err)
		}
		all.Exact = append(all.Exact, pr.Exact...)
		all.Stale = append(all.Stale, pr.Stale...)
		all.Absent = append(all.Absent, pr.Absent...)
	}
	return all, nil
}

// hookPresence is surveyHooks for this binary.
func hookPresence(v adapter.Vendor) (hook.Presence, error) {
	bin, err := aggyBin()
	if err != nil {
		return hook.Presence{}, err
	}
	return surveyHooks(v, bin)
}
