package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/congdoe/aggy/internal/diff"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// Selection picks the changes aggy unwire undoes. Exactly one field is
// set.
type Selection struct {
	ChangeID string // one change
	RunID    string // every change of one wire run
	All      bool   // every change
}

func (s Selection) validate() error {
	n := 0
	if s.ChangeID != "" {
		n++
	}
	if s.RunID != "" {
		n++
	}
	if s.All {
		n++
	}
	if n != 1 {
		return errors.New("manifest: pick exactly one of a change, a run, or all")
	}
	return nil
}

// UnwireOptions changes how Unwire acts.
type UnwireOptions struct {
	// DryRun writes the diffs an undo would make and changes nothing.
	DryRun bool
	// Force undoes a change even when its file changed after aggy changed
	// it. The file's current bytes are first copied into the change's
	// backup folder. It is for one change the person confirmed; the CLI
	// allows it only with --change and --yes.
	Force bool
}

// Outcome is what Unwire did, or in a dry run would do, with one change.
type Outcome string

// Outcomes.
const (
	// Restored: the file's old bytes and mode are back.
	Restored Outcome = "restored"
	// Removed: the file aggy created is gone, with any directories made
	// for it that are now empty.
	Removed Outcome = "removed"
	// Cleaned: a pending or failed change whose write never happened.
	// The file is as it was; any directories made for it that are empty
	// are removed.
	Cleaned Outcome = "cleaned"
	// Drifted: the file changed after aggy changed it, so the change was
	// not undone. Err holds a *patch.DriftError.
	Drifted Outcome = "drifted"
	// Failed: the undo hit another error, in Err.
	Failed Outcome = "failed"
)

// Undo is the result for one change.
type Undo struct {
	Change  store.Change
	Outcome Outcome
	// Saved is where a forced undo copied the file's drifted bytes.
	Saved string
	Err   error
}

// Unwire undoes the selected changes, newest first (reverse id order, as
// store.ChangesNewestFirst returns them): a later change to a file was
// planned against the earlier one's result, so it must be undone first.
// Each change is undone under its target's lock and marked undone.
//
// Which rows it acts on:
//
//   - applied: the file is restored byte for byte from the backup, with
//     its mode, or, when the change created it, removed along with the
//     directories made for it while they are empty.
//   - pending or failed: the row was written before the file, so the
//     write may or may not have happened (a pending row that outlives
//     its process means the process stopped mid-change; the target lock
//     means a live Apply is waited for, not raced). Unwire compares the
//     file with the row's hashes. Still the hash before: nothing was
//     written, and only directories made for a new file are removed while
//     empty (Cleaned). The hash after: the write happened, and it is
//     undone like an applied change, from backups/<change-id>. Anything
//     else is drift. Either way, nothing the change made is left behind.
//   - noop, undone, and marker rows are skipped and not reported.
//
// A file that changed after aggy changed it is not touched: its result
// is Drifted, and Unwire goes on with the other changes. With
// opt.Force it is undone anyway, after its bytes are saved. An earlier
// change to a drifted file will drift as well, since the file no longer
// holds what that change left.
//
// In a dry run, Unwire writes to w the unified diff each undo would make,
// escaped for the terminal, and changes nothing. The returned error joins
// every result's error.
func (m *Manifest) Unwire(ctx context.Context, sel Selection, opt UnwireOptions, w io.Writer) ([]Undo, error) {
	if err := sel.validate(); err != nil {
		return nil, err
	}
	var rows []store.Change
	switch {
	case sel.ChangeID != "":
		c, err := m.store.GetChange(ctx, sel.ChangeID)
		if err != nil {
			return nil, fmt.Errorf("manifest: change %s: %w", sel.ChangeID, err)
		}
		rows = []store.Change{c}
	default:
		var err error
		if rows, err = m.store.ChangesNewestFirst(ctx, sel.RunID); err != nil {
			return nil, err
		}
	}
	var out []Undo
	var errs []error
	for _, c := range rows {
		if !undoable(c) {
			continue
		}
		u := m.undo(ctx, c, opt, w)
		if u.Change.ChangeID == "" {
			continue // no longer undoable once locked
		}
		out = append(out, u)
		if u.Err != nil {
			errs = append(errs, u.Err)
		}
	}
	return out, errors.Join(errs...)
}

// undoable reports whether c is a row Unwire acts on.
func undoable(c store.Change) bool {
	if c.Kind == store.KindMarker {
		return false
	}
	switch c.Status {
	case store.ChangeApplied, store.ChangePending, store.ChangeFailed:
		return true
	}
	return false
}

// undoRecord is the patch.Record that undoes c. A pending or failed row
// has no backup path yet; the backup, if the write went ahead, is where
// Apply puts it.
func (m *Manifest) undoRecord(c store.Change) patch.Record {
	r := Record(c)
	r.Changed = true
	if r.BackupPath == "" && r.BeforeHash != patch.NoFile {
		r.BackupPath = filepath.Join(m.backupRoot, c.ChangeID, r.BeforeHash+".bak")
	}
	return r
}

// undo undoes one change under its target's lock. It returns a zero Undo
// when the row, read again under the lock, is no longer undoable.
func (m *Manifest) undo(ctx context.Context, c store.Change, opt UnwireOptions, w io.Writer) Undo {
	fail := func(err error) Undo { return Undo{Change: c, Outcome: Failed, Err: err} }
	unlock, err := m.lockTarget(ctx, c.File)
	if err != nil {
		return fail(err)
	}
	defer unlock()
	if c, err = m.store.GetChange(ctx, c.ChangeID); err != nil {
		return fail(err)
	}
	if !undoable(c) {
		return Undo{}
	}
	r := m.undoRecord(c)
	cur, curHash, err := readFile(r.File)
	if err != nil {
		return fail(err)
	}
	u := Undo{Change: c}
	switch {
	case curHash == r.BeforeHash:
		u.Outcome = Cleaned
		if c.Status == store.ChangeApplied {
			// Restored by hand, or a half-finished undo.
			u.Outcome = Restored
			if r.BeforeHash == patch.NoFile {
				u.Outcome = Removed
			}
		}
	case curHash == r.AfterHash || opt.Force:
		u.Outcome = Restored
		if r.BeforeHash == patch.NoFile {
			u.Outcome = Removed
		}
	default:
		u.Outcome = Drifted
		u.Err = fmt.Errorf("manifest: change %s: %w", c.ChangeID,
			&patch.DriftError{File: r.File, Expected: r.AfterHash, Actual: curHash})
		if opt.DryRun {
			fmt.Fprintf(w, "# %s: changed since aggy changed it (change %s); would not undo\n",
				termsafe.Line(r.File), termsafe.Line(c.ChangeID))
		}
		return u
	}
	if opt.DryRun {
		if err := previewUndo(w, r, cur, curHash); err != nil {
			return fail(err)
		}
		return u
	}
	if opt.Force && curHash != r.AfterHash {
		u.Saved, err = patch.RevertForce(r, filepath.Join(m.backupRoot, c.ChangeID))
	} else {
		err = patch.Revert(r)
	}
	if err != nil && !errors.Is(err, safefs.ErrDirSync) {
		u.Outcome, u.Err = Failed, err
		return u
	}
	// The file is back; record it even if the caller has given up.
	if merr := m.store.MarkChangeUndone(context.WithoutCancel(ctx), c.ChangeID, m.now()); merr != nil {
		err = errors.Join(merr, err)
	}
	u.Err = err
	return u
}

// readFile returns the file's bytes and hash, or nil and patch.NoFile.
func readFile(path string) ([]byte, string, error) {
	b, err := safefs.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, patch.NoFile, nil
	}
	if err != nil {
		return nil, "", err
	}
	return b, patch.Hash(b), nil
}

// previewUndo writes the diff from the file's current content to what the
// undo leaves, and the directories it would remove.
func previewUndo(w io.Writer, r patch.Record, cur []byte, curHash string) error {
	target, exists, err := patch.Target(r)
	if err != nil {
		return err
	}
	from, to := r.File, r.File
	if curHash == patch.NoFile {
		from = diff.DevNull
	}
	if !exists {
		to = diff.DevNull
	}
	fmt.Fprint(w, termsafe.Lines(diff.Unified(from, to, cur, target)))
	if exists && r.Mode != 0 {
		if fi, err := safefs.Stat(r.File); err == nil && fi.Mode().Perm() != r.Mode {
			fmt.Fprintf(w, "# %s: mode %#o -> %#o\n", termsafe.Line(r.File), fi.Mode().Perm(), r.Mode)
		}
	}
	if !exists {
		for i := len(r.CreatedDirs) - 1; i >= 0; i-- {
			fmt.Fprintf(w, "# %s: would remove the directory if empty\n", termsafe.Line(r.CreatedDirs[i]))
		}
	}
	return nil
}
