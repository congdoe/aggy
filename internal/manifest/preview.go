package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/congdoe/aggy/internal/diff"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// Mode says whether a wire run writes its patches or only shows them.
type Mode int

// Modes.
const (
	// DryRun prints a unified diff of every pending change and writes
	// nothing.
	DryRun Mode = iota
	// Write applies the changes and records them.
	Write
)

func (m Mode) String() string {
	if m == Write {
		return "write"
	}
	return "dry-run"
}

// SetupMode picks the mode for aggy setup. dryRun (--dry-run) always
// previews and write (--write) always writes; both is an error. With
// neither, the first setup previews, so the person sees every config
// change before aggy makes one, and later setups write.
//
// The first setup is one where no setup has ever written: there is no
// index, or it records no applied, no-op, or undone change. A write run
// that finds nothing to change records a no-op row (see Run), so a person
// whose config already matches does not get a dry run forever. SetupMode
// needs no open store: it looks for the index without creating it and
// reads it read-only.
func SetupMode(ctx context.Context, dryRun, write bool) (Mode, error) {
	switch {
	case dryRun && write:
		return DryRun, errors.New("manifest: --dry-run and --write together")
	case dryRun:
		return DryRun, nil
	case write:
		return Write, nil
	}
	path, err := store.DefaultPath()
	if err != nil {
		return DryRun, err
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return DryRun, nil
	} else if err != nil {
		return DryRun, err
	}
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return DryRun, err
	}
	defer func() { _ = s.Close() }()
	v, err := s.Version(ctx)
	if err != nil || v < 1 {
		return DryRun, err
	}
	q := `SELECT EXISTS (SELECT 1 FROM changes WHERE status IN ('applied', 'noop', 'undone'))`
	if v < 2 {
		// Version 1 had no status column; every row was an applied change.
		q = `SELECT EXISTS (SELECT 1 FROM changes)`
	}
	var found bool
	if err := s.DB().QueryRowContext(ctx, q).Scan(&found); err != nil {
		return DryRun, err
	}
	if found {
		return Write, nil
	}
	return DryRun, nil
}

// Preview writes to w a unified diff of each patch that would change a
// file, using patch.Plan, and writes nothing else anywhere. A patch that
// would change nothing is skipped. A patch that would be refused, for
// example because the file changed since it was planned, gets a line
// saying so, and its error is returned at the end, joined with the others.
// It returns the number of files that would change.
//
// The files belong to other tools and may hold anything, so every line
// is escaped with termsafe before it is written: an escape sequence or a
// bidi override is shown, not acted on, and a CR shows as \r.
func Preview(w io.Writer, patches []patch.Patch) (int, error) {
	var errs []error
	changed := 0
	for _, p := range patches {
		pl, err := patch.Plan(p)
		if err != nil {
			fmt.Fprintf(w, "# %s: would not change: %s\n", termsafe.Line(p.File), termsafe.Line(err.Error()))
			errs = append(errs, err)
			continue
		}
		if pl.Action == patch.NoOp {
			continue
		}
		changed++
		if p.Transform == nil {
			fmt.Fprintf(w, "# %s: %s -> %s\n", termsafe.Line(pl.File),
				termsafe.Line(patch.Describe(pl.BeforeHash, pl.Before)), termsafe.Line(patch.Describe(pl.AfterHash, pl.After)))
			continue
		}
		from := pl.File
		if pl.Action == patch.Create {
			from = diff.DevNull
		}
		fmt.Fprint(w, termsafe.Lines(diff.Unified(from, pl.File, pl.Before, pl.After)))
	}
	fmt.Fprintf(w, "# %d %s would change\n", changed, plural(changed, "file", "files"))
	return changed, errors.Join(errs...)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Run applies patches in mode. In DryRun it calls Preview and returns no
// changes. In Write it applies each patch as part of run runID, stopping at
// the first refusal, and returns the changes made so far. A write run that
// records no change records one no-op row for the run. A directory sync
// warning does not stop the run; the warnings are joined into the error.
func (m *Manifest) Run(ctx context.Context, mode Mode, runID string, patches []patch.Patch, w io.Writer) ([]store.Change, error) {
	if mode == DryRun {
		_, err := Preview(w, patches)
		return nil, err
	}
	var out []store.Change
	var warns []error
	recorded := false
	for _, p := range patches {
		c, err := m.Apply(ctx, runID, p)
		if c.ChangeID != "" || c.Status == store.ChangeNoop {
			out = append(out, c)
		}
		recorded = recorded || c.ChangeID != ""
		if err != nil && !isSyncWarning(err) {
			return out, errors.Join(append(warns, err)...)
		}
		if err != nil {
			warns = append(warns, err)
		}
	}
	if !recorded {
		// Nothing needed changing. Record that a write run happened, so
		// SetupMode stops treating the next setup as the first. It is a
		// marker row, which aggy unwire skips.
		now := m.now()
		if err := m.store.InsertChange(ctx, store.Change{
			ChangeID: newID(now), RunID: runID, Status: store.ChangeNoop, Kind: store.KindMarker, PlannedAt: now, AppliedAt: now,
		}); err != nil {
			warns = append(warns, err)
		}
	}
	return out, errors.Join(warns...)
}
