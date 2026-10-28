package manifest

import (
	"context"
	"io"

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
	out := make([]Drift, 0, len(undos))
	for i := len(undos) - 1; i >= 0; i-- {
		u := undos[i]
		d := Drift{Change: u.Change, Outcome: u.Outcome, Drifted: u.Outcome == Drifted}
		switch u.Outcome {
		case Failed, Invalid, Blocked:
			d.Err = u.Err
		}
		out = append(out, d)
	}
	return out, nil
}
