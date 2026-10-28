package manifest

import (
	"context"

	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// Drift is the state of one file aggy changed whose change is still in
// effect.
type Drift struct {
	Change store.Change // the newest change to the file that unwire would act on
	// Drifted: aggy unwire would refuse to undo the change without
	// --force, because the file changed after aggy changed it.
	Drifted bool
	// Err means the file could not be checked: the row is not one aggy
	// could have written, the path is a credential file, or reading
	// failed. Drifted is then false.
	Err error
}

// Drift reports, for each file aggy changed, whether aggy unwire would
// call it drifted: refuse to undo it without --force.
//
// It decides as Unwire does for the newest change to a file that Unwire
// acts on (applied, pending, or failed; Unwire undoes newest first, so
// that one is reached before any older change to the file). The file is
// not drifted when it holds the change's hash before (restored by hand,
// or never written; a created file removed counts) or after (as aggy
// left it), or aggy's change undone by hand as Unwire accepts it
// (userReverted). Anything else is drifted.
//
// TODO(T6612): run Unwire{All: true} as a dry run and map its outcomes,
// so there is one classifier. That needs a dry run that takes no target
// locks: lockTarget creates run/patch/ and a lock file per target, which
// a read-only command must not.
//
// Files are read through safefs, so a credential path is never read: a
// row naming one is reported with an error. Spellings of one file
// (through a symlinked directory, or in another case where the file
// system ignores case) are one target. Results are in the order of each
// target's first change. It writes nothing.
func (m *Manifest) Drift(ctx context.Context) ([]Drift, error) {
	cs, err := m.store.Changes(ctx, "")
	if err != nil {
		return nil, err
	}
	latest := map[string]int{} // target key -> index into cs
	var order []string
	for i, c := range cs {
		if !undoable(c) {
			continue
		}
		key, err := safefs.CanonicalPath(c.File)
		if err != nil {
			key = c.File
		}
		if _, ok := latest[key]; !ok {
			order = append(order, key)
		}
		latest[key] = i
	}
	var out []Drift
	for _, key := range order {
		c := cs[latest[key]]
		d := Drift{Change: c}
		if err := m.validRow(c); err != nil {
			d.Err = err
		} else if err := safefs.Check(c.File); err != nil {
			d.Err = err
		} else if cur, curHash, err := readFile(c.File); err != nil {
			d.Err = err
		} else {
			r := m.undoRecord(c)
			d.Drifted = curHash != r.BeforeHash && curHash != r.AfterHash && !userReverted(r, cur, curHash)
		}
		out = append(out, d)
	}
	return out, nil
}
