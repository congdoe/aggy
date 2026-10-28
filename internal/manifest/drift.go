package manifest

import (
	"context"
	"io"

	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// Drift is what aggy unwire --all would do with one change, for aggy
// doctor.
type Drift struct {
	Change  store.Change
	Outcome Outcome // the Unwire dry-run outcome
	// Drifted: the file changed after aggy changed it, so aggy unwire
	// would ask for --force (Outcome Drifted).
	Drifted bool
	// Err is the outcome's error: why a change is Failed (a credential
	// path, an unreadable file), Invalid, or Blocked.
	Err error
	// BlockedBy, for a Blocked change, is the id of the nearest newer
	// change to the same file that the run leaves in place (Drifted,
	// Failed, Invalid, or Blocked itself), the one in its way, or "" when
	// none is in the results.
	BlockedBy string
}

// Drift runs Unwire{All: true} as a dry run that takes no locks and
// writes nothing (UnwireOptions.NoLock), and returns each change's
// outcome, oldest first. So doctor and unwire cannot disagree: a change
// has drifted exactly when unwire would refuse it without --force.
// Files are read through safefs, as Unwire reads them, so a credential
// path is never read: it comes back Failed.
func (m *Manifest) Drift(ctx context.Context) ([]Drift, error) {
	undos, err := m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true, NoLock: true}, io.Discard)
	if len(undos) == 0 && err != nil {
		return nil, err
	}
	key := func(file string) string {
		if k, err := safefs.CanonicalPath(file); err == nil {
			return k
		}
		return file
	}
	out := make([]Drift, 0, len(undos))
	for i := len(undos) - 1; i >= 0; i-- {
		u := undos[i]
		d := Drift{Change: u.Change, Outcome: u.Outcome, Drifted: u.Outcome == Drifted}
		switch u.Outcome {
		case Failed, Invalid, Blocked:
			d.Err = u.Err
		}
		if u.Outcome == Blocked {
			// undos is newest first: the newer changes come before i. A
			// newer change the dry run resolved (restored, removed,
			// cleaned, superseded) is out of the way; the blocker is the
			// nearest one left in place.
			for j := i - 1; j >= 0; j-- {
				n := undos[j]
				if key(n.Change.File) == key(u.Change.File) && unresolved(n.Outcome) {
					d.BlockedBy = n.Change.ChangeID
					break
				}
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// unresolved reports whether a dry run leaves a change in place.
func unresolved(o Outcome) bool {
	switch o {
	case Drifted, Failed, Invalid, Blocked:
		return true
	}
	return false
}
