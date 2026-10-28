package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"

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
reports it and goes on with the rest.
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
		fmt.Fprintf(stderr, "aggy unwire: %v\n\n%s", err, unwireUsage)
		return 2
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
		fmt.Fprintf(stderr, "aggy unwire: unexpected arguments\n\n%s", unwireUsage)
		return 2
	case n != 1:
		fmt.Fprintf(stderr, "aggy unwire: pick exactly one of --change, --run, or --all\n\n%s", unwireUsage)
		return 2
	case *force && *change == "":
		fmt.Fprintf(stderr, "aggy unwire: --force undoes one change at a time; use it with --change\n")
		return 2
	case *yes && !*force:
		fmt.Fprintf(stderr, "aggy unwire: --yes only confirms --force\n")
		return 2
	}
	// --force without --yes is a dry run of the forced undo.
	opt := manifest.UnwireOptions{DryRun: *dryRun || (*force && !*yes), Force: *force}

	path, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "aggy unwire: %v\n", err)
		return 1
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stdout, "Nothing to undo: aggy has not changed any config.")
		return 0
	} else if err != nil {
		fmt.Fprintf(stderr, "aggy unwire: %v\n", err)
		return 1
	}
	ctx := context.Background()
	s, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintf(stderr, "aggy unwire: %v\n", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		fmt.Fprintf(stderr, "aggy unwire: %v\n", err)
		return 1
	}
	undos, err := m.Unwire(ctx, sel, opt, stdout)
	if len(undos) == 0 && err != nil {
		fmt.Fprintf(stderr, "aggy unwire: %s\n", termsafe.Line(err.Error()))
		return 1
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
	for _, u := range undos {
		file, id := termsafe.Field(u.Change.File), termsafe.Field(u.Change.ChangeID)
		switch u.Outcome {
		case manifest.Drifted:
			code = 1
			fmt.Fprintf(stderr, "refused %s (change %s): it changed after aggy changed it. Undo it by hand, or run aggy unwire --change %s --force to see what forcing would overwrite.\n", file, id, id)
		case manifest.Failed:
			code = 1
			fmt.Fprintf(stderr, "failed %s (change %s): %s\n", file, id, termsafe.Field(u.Err.Error()))
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
	if unconfirmed {
		fmt.Fprintln(stderr, "Nothing changed. Run again with --force --yes to overwrite the edits shown above.")
		return 1
	}
	fmt.Fprintf(stdout, "%d of %d %s %sundone.\n", done, len(undos), plural(len(undos), "change", "changes"), would)
	return code
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
