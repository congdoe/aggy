package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

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
	undos, err := m.Unwire(ctx, sel, opt, stdout)
	if len(undos) == 0 && err != nil {
		code := diag.CodeUnwireFailed
		if errors.Is(err, store.ErrNotFound) {
			code = diag.CodeNotFound
		}
		return cmdErrDry(stderr, "unwire", code, err, opt.DryRun)
	}
	return report(undos, opt, *force && !*yes, stdout, stderr)
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
	if unconfirmed && !onlyBlocked(undos) {
		fmt.Fprintln(stderr, "Nothing changed. Run again with --force --yes to overwrite the edits shown above.")
		return 1
	}
	fmt.Fprintf(stdout, "%d of %d %s %sundone.\n", done, len(undos), plural(len(undos), "change", "changes"), would)
	return code
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
