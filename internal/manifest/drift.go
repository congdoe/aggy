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
	// BlockedBy, for a Blocked change, is the id of the next newer
	// change to the same file, the one in its way (in an --all run, one
	// that is itself not undone, such as a Drifted one), or "" when it is
	// not in the results.
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
			// undos is newest first: the newer changes come before i.
			for j := i - 1; j >= 0; j-- {
				if key(undos[j].Change.File) == key(u.Change.File) {
					d.BlockedBy = undos[j].Change.ChangeID
					break
				}
			}
		}
		out = append(out, d)
	}
	return out, nil
}
