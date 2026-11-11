package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// unwireGate is personGate; the package's other tests undo things without a
// terminal, and TestUnwireIsForThePerson puts the real one back.
var unwireGate = personGate

const unwireUsage = `Usage: aggy unwire (--change ID | --run ID | --all) [--dry-run]
       aggy unwire --change ID --force [--yes]

Undoes config changes aggy made to other tools, newest first: each file
gets its old bytes and mode back, and a file aggy created is removed with
any directories made for it that are now empty.

  --change ID   undo one change
  --run ID      undo every change of one run
  --all         undo every change
  --dry-run     show the diff of each undo and change nothing
  --force       undo a change whose file was edited after aggy changed it;
                the edited bytes are saved under ~/.aggy/backups first.
                Only with --change. Without --yes it shows what would be
                overwritten and changes nothing.
  --yes         confirm --force

A file edited after aggy changed it is not undone without --force; unwire
reports it and goes on with the rest. A change cannot be undone with
--change while a newer change to the same file is in place: undo that
one first, or use --all.

Undoing can bring an allow back or take a deny away, so it is for you, at a
terminal; a session's tool shell is refused, and --dry-run stays open. That
holds against mistakes, not a model that tries: it can give itself a terminal
with script, and pipe y.
`

// runUnwire is aggy unwire. It never creates the store: with no index
// there is nothing to undo.
func runUnwire(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("unwire", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	change := fl.String("change", "", "")
	run := fl.String("run", "", "")
	all := fl.Bool("all", false, "")
	dryRun := fl.Bool("dry-run", false, "")
	force := fl.Bool("force", false, "")
	yes := fl.Bool("yes", false, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, unwireUsage)
			return 0
		}
		return cmdUsage(stderr, "unwire", err.Error(), unwireUsage)
	}
	sel := manifest.Selection{ChangeID: *change, RunID: *run, All: *all}
	n := 0
	for _, set := range []bool{*change != "", *run != "", *all} {
		if set {
			n++
		}
	}
	switch {
	case fl.NArg() > 0:
		return cmdUsage(stderr, "unwire", "unexpected arguments", unwireUsage)
	case n != 1:
		return cmdUsage(stderr, "unwire", "pick exactly one of --change, --run, or --all", unwireUsage)
	case *force && *change == "":
		return cmdUsage(stderr, "unwire", "--force undoes one change at a time; use it with --change", "")
	case *yes && !*force:
		return cmdUsage(stderr, "unwire", "--yes only confirms --force", "")
	}
	// --force without --yes is a dry run of the forced undo.
	opt := manifest.UnwireOptions{DryRun: *dryRun || (*force && !*yes), Force: *force}
	// Undoing can bring an allow back, or take a deny or a trust away, so it is
	// for the person, like making the change. A preview stays open.
	if !opt.DryRun {
		if err := unwireGate(os.Getenv); err != nil {
			return cmdErr(stderr, "unwire", diag.CodeNotAtKeyboard, err)
		}
	}

	path, err := store.DefaultPath()
	if err != nil {
		return cmdErr(stderr, "unwire", diag.CodeStore, err)
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if *change != "" || *run != "" {
			return cmdErrDry(stderr, "unwire", diag.CodeNotFound, errors.New("no such change: aggy has not changed any config"), opt.DryRun)
		}
		fmt.Fprintln(stdout, "Nothing to undo: aggy has not changed any config.")
		return 0
	} else if err != nil {
		return cmdErr(stderr, "unwire", diag.CodeStore, err)
	}
	ctx := context.Background()
	s, err := store.Open(ctx, path)
	if err != nil {
		return cmdErr(stderr, "unwire", diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return cmdErr(stderr, "unwire", diag.CodeStore, err)
	}
	// A Hermes profile aggy made (#134 review) is a marker, which the
	// manifest skips: it is deleted after the changes, so its block is
	// undone first.
	profiles, err := hermesProfileMarkers(ctx, s, sel)
	if err != nil {
		return cmdErr(stderr, "unwire", diag.CodeStore, err)
	}
	if sel.ChangeID != "" && len(profiles) == 1 {
		if *force {
			return cmdUsage(stderr, "unwire", "--force does not apply to a Hermes profile aggy made", "")
		}
		if _, left := unwireHermesProfiles(ctx, s, profiles, opt.DryRun, stdout, stderr); left {
			return 1
		}
		return 0
	}
	undos, err := m.Unwire(ctx, sel, opt, stdout)
	if len(undos) == 0 && err != nil && len(profiles) == 0 {
		code := diag.CodeUnwireFailed
		if errors.Is(err, store.ErrNotFound) {
			code = diag.CodeNotFound
		}
		return cmdErrDry(stderr, "unwire", code, err, opt.DryRun)
	}
	code := report(undos, opt, *force && !*yes, stdout, stderr)
	if len(profiles) > 0 && (!*force || *yes) {
		if _, left := unwireHermesProfiles(ctx, s, profiles, opt.DryRun, stdout, stderr); left {
			code = 1
		}
	}
	return code
}

// report prints one line per change and returns the exit code: 0 when
// every change was undone (or would be), 1 otherwise.
func report(undos []manifest.Undo, opt manifest.UnwireOptions, unconfirmed bool, stdout, stderr io.Writer) int {
	if len(undos) == 0 {
		fmt.Fprintln(stdout, "Nothing to undo.")
		return 0
	}
	would := ""
	if opt.DryRun {
		would = "would be "
	}
	code := 0
	done := 0
	// A record for each refused or failed change, unless this is a dry run.
	note := func(c diag.Code, line string) {
		code = 1
		tag := ""
		if w := writerFor(opt.DryRun); w != nil {
			w.Log(context.Background(), diag.Rec{Event: diag.EvCommandRefused, Code: c, Msg: fixedMsg(c)})
			tag = " " + w.Tag(c)
		}
		fmt.Fprintln(stderr, line+tag)
	}
	for _, u := range undos {
		file, id := termsafe.Field(u.Change.File), termsafe.Field(u.Change.ChangeID)
		switch u.Outcome {
		case manifest.Drifted:
			note(diag.CodeUnwireRefused, fmt.Sprintf("refused %s (change %s): it changed after aggy changed it. Undo it by hand, or run aggy unwire --change %s --force to see what forcing would overwrite.", file, id, id))
		case manifest.Blocked:
			note(diag.CodeUnwireRefused, fmt.Sprintf("refused %s (change %s): %s", file, id, termsafe.Field(u.Err.Error())))
		case manifest.Invalid:
			note(diag.CodeUnwireRefused, fmt.Sprintf("refused change %s: %s", id, termsafe.Field(u.Err.Error())))
		case manifest.Failed:
			note(diag.CodeUnwireFailed, fmt.Sprintf("failed %s (change %s): %s", file, id, termsafe.Field(u.Err.Error())))
		case manifest.Superseded:
			done++
			fmt.Fprintf(stdout, "%sundone %s (change %s): a newer change wrote the file since, so the file is left as it is\n", would, file, id)
		default:
			done++
			fmt.Fprintf(stdout, "%s%s %s (change %s)\n", would, u.Outcome, file, id)
			if u.Saved != "" {
				fmt.Fprintf(stdout, "  its edited bytes are saved in %s\n", termsafe.Field(u.Saved))
			}
			if u.Err != nil {
				// A sync warning: the file is restored.
				fmt.Fprintf(stderr, "warning: %s\n", termsafe.Field(u.Err.Error()))
			}
		}
	}
	if line := trustHalfUndone(undos); line != "" {
		fmt.Fprintln(stdout, line)
	}
	if unconfirmed && !onlyBlocked(undos) {
		fmt.Fprintln(stderr, "Nothing changed. Run again with --force --yes to overwrite the edits shown above.")
		return 1
	}
	fmt.Fprintf(stdout, "%d of %d %s %sundone.\n", done, len(undos), plural(len(undos), "change", "changes"), would)
	return code
}

// trustHalfUndone speaks up when aggy trust's record went but Codex's half stayed,
// since Codex's config changed after: aggy now says untrusted and Codex does not.
func trustHalfUndone(undos []manifest.Undo) string {
	path, err := parity.CodexConfigPath()
	if err != nil {
		return ""
	}
	trust, err := trustFile()
	if err != nil {
		return ""
	}
	var codex *manifest.Undo
	undone := false
	for i, u := range undos {
		switch {
		case u.Change.File == path && (u.Outcome == manifest.Drifted || u.Outcome == manifest.Blocked):
			codex = &undos[i]
		case u.Change.File == trust && u.Outcome != manifest.Drifted && u.Outcome != manifest.Blocked && u.Outcome != manifest.Invalid && u.Outcome != manifest.Failed:
			undone = true
		}
	}
	if codex == nil || !undone {
		return ""
	}
	id := termsafe.Field(codex.Change.ChangeID)
	return fmt.Sprintf("aggy no longer trusts this repo, but Codex still may: its config changed after aggy trust, so aggy left it. Remove trust_level under the repo's [projects] entry in %s by hand, or run aggy unwire --change %s --force to see what forcing would overwrite.", termsafe.Field(path), id)
}

// onlyBlocked reports whether every result is Blocked or Invalid, which
// --force cannot change.
func onlyBlocked(undos []manifest.Undo) bool {
	for _, u := range undos {
		if u.Outcome != manifest.Blocked && u.Outcome != manifest.Invalid {
			return false
		}
	}
	return true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
