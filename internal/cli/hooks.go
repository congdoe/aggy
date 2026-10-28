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
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const hooksUsage = `Usage: aggy hooks install [--vendor V] [--dry-run]
       aggy hooks status

Installs the hooks that tell aggy when a vendor CLI's session starts, its
turn ends, or it hits a limit. Each hook runs this aggy binary, by its
absolute path, as aggy hook --vendor V <event>; outside aggy it does
nothing. Existing hooks are kept. Every change goes through the
manifest; aggy unwire undoes it.

  claude   ~/.claude/settings.json: Stop, StopFailure, SessionStart, Notification

  --vendor V   install for one vendor only (default: all)
  --dry-run    show the changes and make none
`

// hookBin is the aggy binary hooks call; tests replace it.
var hookBin = os.Executable

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
// CLAUDE_CONFIG_DIR is unverified, as for skills, so not followed).
func hookFiles(v adapter.Vendor) ([]hookFile, error) {
	h := os.Getenv("HOME")
	if !filepath.IsAbs(h) {
		return nil, errors.New("HOME is not an absolute path")
	}
	switch v {
	case adapter.Claude:
		return []hookFile{{filepath.Join(h, ".claude", "settings.json"),
			entries(v, "Stop", "StopFailure", "SessionStart", "Notification")}}, nil
	}
	return nil, nil
}

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
		if len(missing) > 0 {
			p.patches = append(p.patches, patch.Patch{File: f.path, ExpectedHash: h, Transform: hook.Merge(bin, f.entries)})
		}
	}
	return p, nil
}

// aggyBin returns the path hooks should call: this binary's.
func aggyBin() (string, error) {
	bin, err := hookBin()
	if err != nil {
		return "", err
	}
	if strings.Contains(bin, string(filepath.Separator)+"go-build") {
		return "", errors.New("aggy is running from go run's temporary build; install it (make build) and run that binary")
	}
	return bin, hook.CheckBin(bin)
}

func runHooks(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, hooksUsage)
		return 0
	}
	op := args[0]
	fl := flag.NewFlagSet("hooks", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	vendor := fl.String("vendor", "", "")
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy hooks: bad arguments\n\n%s", hooksUsage)
		return 2
	}
	vendors := adapter.Vendors()
	if *vendor != "" {
		v, err := adapter.ParseVendor(*vendor)
		if err != nil {
			fmt.Fprintf(stderr, "aggy hooks: %v\n", err)
			return 2
		}
		vendors = []adapter.Vendor{v}
	}
	bin, err := aggyBin()
	if err != nil {
		fmt.Fprintf(stderr, "aggy hooks: %s\n", termsafe.Line(err.Error()))
		return 1
	}
	switch op {
	case "status":
		if *vendor != "" || *dryRun {
			fmt.Fprintln(stderr, "aggy hooks status takes no flags")
			return 2
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
	case "install":
	default:
		fmt.Fprintf(stderr, "aggy hooks: unknown subcommand %q\n\n%s", op, hooksUsage)
		return 2
	}
	var ps []patch.Patch
	var notes []string
	for _, v := range vendors {
		p, err := planHooks(v, bin)
		if err != nil {
			fmt.Fprintf(stderr, "aggy hooks install: %s: %s\n", v, termsafe.Line(err.Error()))
			return 1
		}
		ps = append(ps, p.patches...)
		if len(p.patches) > 0 && p.note != "" {
			notes = append(notes, p.note)
		}
	}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	if len(ps) == 0 {
		fmt.Fprintln(stdout, "Nothing to change: aggy's hooks are installed.")
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy hooks: %v\n", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		fmt.Fprintf(stderr, "aggy hooks: %v\n", err)
		return 1
	}
	runID := manifest.NewRunID()
	cs, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard)
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			fmt.Fprintf(stdout, "changed %s\n", termsafe.Field(c.File))
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy hooks install: %s\n", termsafe.Line(err.Error()))
		return 1
	}
	for _, n := range notes {
		fmt.Fprintln(stdout, n)
	}
	fmt.Fprintf(stdout, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return 0
}

// Hook states, as aggy hooks status and aggy doctor show them.
const (
	hookInstalled = "installed"
	hookMissing   = "not installed (aggy hooks install)"
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
	case len(p.patches) > 0:
		return hookMissing, ""
	}
	return hookInstalled, ""
}
