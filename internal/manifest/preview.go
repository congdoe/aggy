package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/congdoe/aggy/internal/diff"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
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
// neither, the first setup, before any change has been applied, previews,
// so the person sees every config change before aggy makes one. Later
// setups write.
func (m *Manifest) SetupMode(ctx context.Context, dryRun, write bool) (Mode, error) {
	switch {
	case dryRun && write:
		return DryRun, errors.New("manifest: --dry-run and --write together")
	case dryRun:
		return DryRun, nil
	case write:
		return Write, nil
	}
	cs, err := m.store.Changes(ctx, "")
	if err != nil {
		return DryRun, err
	}
	for _, c := range cs {
		if c.Status == store.ChangeApplied || c.Status == store.ChangeUndone {
			return Write, nil
		}
	}
	return DryRun, nil
}

// Preview writes to w a unified diff of each patch that would change a
// file, using patch.Plan, and writes nothing else anywhere. A patch that
// would change nothing is skipped. A patch that would be refused, for
// example because the file changed since it was planned, gets a line
// saying so, and its error is returned at the end, joined with the others.
// It returns the number of files that would change.
func Preview(w io.Writer, patches []patch.Patch) (int, error) {
	var errs []error
	changed := 0
	for _, p := range patches {
		pl, err := patch.Plan(p)
		if err != nil {
			fmt.Fprintf(w, "# %s: would not change: %v\n", p.File, err)
			errs = append(errs, err)
			continue
		}
		if pl.Action == patch.NoOp {
			continue
		}
		changed++
		from := pl.File
		if pl.Action == patch.Create {
			from = diff.DevNull
		}
		fmt.Fprint(w, diff.Unified(from, pl.File, pl.Before, pl.After))
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
// the first refusal, and returns the changes made so far. A directory sync
// warning does not stop the run; the warnings are joined into the error.
func (m *Manifest) Run(ctx context.Context, mode Mode, runID string, patches []patch.Patch, w io.Writer) ([]store.Change, error) {
	if mode == DryRun {
		_, err := Preview(w, patches)
		return nil, err
	}
	var out []store.Change
	var warns []error
	for _, p := range patches {
		c, err := m.Apply(ctx, runID, p)
		if c.ChangeID != "" || c.Status == store.ChangeNoop {
			out = append(out, c)
		}
		if err != nil && !isSyncWarning(err) {
			return out, errors.Join(append(warns, err)...)
		}
		if err != nil {
			warns = append(warns, err)
		}
	}
	return out, errors.Join(warns...)
}
