package manifest

import (
	"context"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// Drift is the state of one file aggy changed whose change is still in
// effect.
type Drift struct {
	Change  store.Change // the newest applied or pending change to the file
	Drifted bool         // the file no longer holds what aggy last wrote
	// Err means the file could not be checked: the row is not one aggy
	// could have written, the path is a credential file, or reading
	// failed. Drifted is then false.
	Err error
}

// Drift compares each file aggy changed with what aggy last wrote to it.
// For every target, the newest change that is applied or pending (not
// undone, failed, or a no-op) is the one in effect. An applied change is
// drifted when the file's hash is not its hash after; a pending one,
// whose write may or may not have happened, when the hash is neither its
// hash before nor after. A missing file has the hash patch.NoFile, so a
// removed file aggy created or changed counts as drifted.
//
// Files are read through safefs, so a credential path is never read: a
// row naming one is reported with an error. Spellings of one file
// (through a symlinked directory, or in another case where the file
// system ignores case) are compared as one target. Results are in the
// order of the changes' ids.
func (m *Manifest) Drift(ctx context.Context) ([]Drift, error) {
	cs, err := m.store.Changes(ctx, "")
	if err != nil {
		return nil, err
	}
	latest := map[string]int{} // target key -> index into cs
	var order []string
	for i, c := range cs {
		if c.Kind == store.KindMarker || (c.Status != store.ChangeApplied && c.Status != store.ChangePending) {
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
		} else if cur, err := patch.HashFile(c.File); err != nil {
			d.Err = err
		} else {
			d.Drifted = cur != c.HashAfter && (c.Status != store.ChangePending || cur != c.HashBefore)
		}
		out = append(out, d)
	}
	return out, nil
}
