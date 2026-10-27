// Package manifest applies config patches and records each change in the
// store's changes table, the manifest that aggy unwire reads. Before a file
// is written, its old bytes are copied to backups/<change-id> in the state
// directory ($AGGY_HOME or ~/.aggy), and a pending row records the hashes
// the change expects, so even a crash mid-change leaves enough to undo it.
package manifest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// BackupDirName is the backups folder inside the state directory.
const BackupDirName = "backups"

// Manifest applies patches and records them.
type Manifest struct {
	store      *store.Store
	backupRoot string
	now        func() time.Time
}

// New returns a Manifest that records in s and keeps backups under
// backupRoot/<change-id>.
func New(s *store.Store, backupRoot string) *Manifest {
	return &Manifest{store: s, backupRoot: backupRoot, now: time.Now}
}

// Open returns a Manifest with backups in the state directory.
func Open(s *store.Store) (*Manifest, error) {
	dir, err := home.Dir()
	if err != nil {
		return nil, err
	}
	return New(s, filepath.Join(dir, BackupDirName)), nil
}

// newID returns a sortable, path-safe id: UTC time then random hex.
func newID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// NewRunID returns an id grouping the changes of one wire run, so they can
// be listed and undone together.
func NewRunID() string { return "run-" + newID(time.Now()) }

// Apply applies p as part of run runID and records it.
//
// A patch that would change nothing is not recorded and comes back with
// Status store.ChangeNoop. A refused patch (drift, a credential path, a
// transform error) records nothing. Otherwise a pending row is written
// first, then the backup and the file, then the row is marked applied with
// the undo facts. When only the directory sync after the write failed, the
// change is recorded as applied and the error matching safefs.ErrDirSync
// is returned with it.
func (m *Manifest) Apply(ctx context.Context, runID string, p patch.Patch) (store.Change, error) {
	pl, err := patch.Plan(p)
	if err != nil {
		return store.Change{}, err
	}
	c := store.Change{
		RunID:       runID,
		File:        pl.File,
		HashBefore:  pl.BeforeHash,
		HashAfter:   pl.AfterHash,
		CreatedFile: pl.Created,
		FileMode:    pl.Mode,
	}
	if pl.Action == patch.NoOp {
		c.Status = store.ChangeNoop
		return c, nil
	}
	c.PlannedAt = m.now()
	c.ChangeID = newID(c.PlannedAt)
	if err := m.store.InsertChange(ctx, c); err != nil {
		return store.Change{}, fmt.Errorf("manifest: record %s: %w", c.File, err)
	}
	rec, err := patch.Apply(p, filepath.Join(m.backupRoot, c.ChangeID))
	warn := err
	if err != nil && !errors.Is(err, safefs.ErrDirSync) {
		c.Status = store.ChangeFailed
		if ferr := m.store.FinishChange(context.WithoutCancel(ctx), c); ferr != nil {
			err = errors.Join(err, ferr)
		}
		return store.Change{}, err
	}
	c.Status = store.ChangeNoop
	if rec.Changed {
		c.Status = store.ChangeApplied
	}
	c.HashBefore, c.HashAfter, c.BackupPath = rec.BeforeHash, rec.AfterHash, rec.BackupPath
	c.CreatedFile, c.FileMode, c.CreatedDirs = rec.Created, rec.Mode, rec.CreatedDirs
	c.AppliedAt = m.now()
	// The file has changed: record it even if the caller has given up.
	if err := m.store.FinishChange(context.WithoutCancel(ctx), c); err != nil {
		return c, errors.Join(fmt.Errorf("manifest: %s changed but the manifest still says pending: %w", c.File, err), warn)
	}
	got, err := m.store.GetChange(context.WithoutCancel(ctx), c.ChangeID)
	if err != nil {
		return c, errors.Join(err, warn)
	}
	return got, warn
}

// Record converts a recorded change back to the patch.Record that
// patch.Revert takes.
func Record(c store.Change) patch.Record {
	return patch.Record{
		File:        c.File,
		BeforeHash:  c.HashBefore,
		AfterHash:   c.HashAfter,
		BackupPath:  c.BackupPath,
		Changed:     c.Status == store.ChangeApplied,
		Created:     c.CreatedFile,
		Mode:        c.FileMode,
		CreatedDirs: c.CreatedDirs,
	}
}

// Revert undoes one applied change and marks it undone. It refuses, with a
// *patch.DriftError, when the file changed after aggy changed it.
func (m *Manifest) Revert(ctx context.Context, changeID string) error {
	c, err := m.store.GetChange(ctx, changeID)
	if err != nil {
		return err
	}
	if c.Status != store.ChangeApplied {
		return fmt.Errorf("manifest: change %s is %s, not applied", changeID, c.Status)
	}
	err = patch.Revert(Record(c))
	if err != nil && !errors.Is(err, safefs.ErrDirSync) {
		return err
	}
	if merr := m.store.MarkChangeUndone(context.WithoutCancel(ctx), changeID, m.now()); merr != nil {
		return errors.Join(merr, err)
	}
	return err
}
