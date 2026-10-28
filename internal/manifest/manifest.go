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
	"io"
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

// lockWait bounds the wait for another process changing the same file.
const lockWait = 30 * time.Second

// pathKey is the one spelling of a change's path that every other
// spelling shares: safefs.CanonicalPath, which resolves symlinks through
// the deepest existing ancestor and normalizes and case-folds where the
// file system ignores case. For a link or removal change (noFollow) the
// final element is not followed, since the change is to the link itself:
// its directory is made canonical and its base name kept. Two links to
// one target are then two paths, as they are on disk.
//
// shortcut: a noFollow key keeps the base name's case, so on a
// case-insensitive file system two spellings of one link differ; aggy
// spells its own link paths one way.
func pathKey(file string, noFollow bool) (string, error) {
	if !noFollow {
		return safefs.CanonicalPath(file)
	}
	dir, err := safefs.CanonicalPath(filepath.Dir(file))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(file)), nil
}

// lockPath is the per-target lock file: run/patch/<sha256(key)>.lock next
// to the database. The key is the target's pathKey, so every spelling of
// one file, through a linked directory or in another case, takes the same
// lock.
func (m *Manifest) lockPath(file string, noFollow bool) (string, error) {
	key, err := pathKey(file, noFollow)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(m.store.Path()), "run", "patch", patch.Hash([]byte(key))+".lock"), nil
}

// lockFile is store.LockFile; tests replace it to simulate a file system
// without locks.
var lockFile = store.LockFile

// lockTarget takes the per-target lock. Where file locks are unsupported
// it goes on unlocked, and locked is false; the hash guard then still
// refuses most races, and Apply marks the row Unlocked, so unwire does
// not take its plan as proof about an older change.
func (m *Manifest) lockTarget(ctx context.Context, file string, noFollow bool) (unlock func(), locked bool, err error) {
	lock, err := m.lockPath(file, noFollow)
	if err != nil {
		return nil, false, err
	}
	if err := safefs.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return nil, false, err
	}
	unlock, err = lockFile(ctx, lock, lockWait)
	if errors.Is(err, store.ErrLockUnsupported) {
		return func() {}, false, nil
	}
	return unlock, err == nil, err
}

// Apply applies p as part of run runID and records it.
//
// It holds a lock on the target file from the plan to the final manifest
// update, so two processes changing one file cannot both pass the hash
// check and lose an update. A patch that would change nothing is not
// recorded and comes back with Status store.ChangeNoop. A patch refused
// at the plan (drift, a credential path, a transform error) records
// nothing. Otherwise a pending row is written first, holding the expected hashes
// and the directories a new file needs, then the backup and the file, then
// the row is marked applied with the undo facts, or failed with the
// directories patch.Apply made itself before it failed. A failed row never
// wrote the file: that includes drift found by patch.Apply's re-check
// just before the write, which leaves a failed row with no backup. When only the directory
// sync after the target write failed, the change is recorded as applied
// and the error matching patch.ErrNotSynced is returned with it.
func (m *Manifest) Apply(ctx context.Context, runID string, p patch.Patch) (store.Change, error) {
	file, err := filepath.Abs(p.File)
	if err != nil {
		return store.Change{}, err
	}
	unlock, locked, err := m.lockTarget(ctx, file, p.Transform == nil)
	if err != nil {
		return store.Change{}, err
	}
	defer unlock()
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
		CreatedDirs: pl.CreatedDirs,
		Unlocked:    !locked,
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
	if err != nil && (!rec.Changed || !errors.Is(err, patch.ErrNotSynced)) {
		c.Status = store.ChangeFailed
		// Only the directories this Apply made, never one another
		// process made in the meantime.
		c.CreatedDirs = rec.CreatedDirs
		c.CreatedDirIDs = dirIDs(c.CreatedDirs)
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
	c.CreatedFile, c.FileMode = rec.Created, rec.Mode
	if rec.Changed {
		c.CreatedDirs = rec.CreatedDirs
	}
	c.CreatedDirIDs = dirIDs(c.CreatedDirs)
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

// dirIDs returns the identity of each directory in dirs that exists now,
// and a zero DirID for one that does not. The sweep in Unwire removes a
// created directory only while it still has the identity recorded here,
// so a directory someone made later at the same path is kept.
func dirIDs(dirs []string) []store.DirID {
	if len(dirs) == 0 {
		return nil
	}
	ids := make([]store.DirID, len(dirs))
	for i, d := range dirs {
		ids[i], _ = dirIdentity(d)
	}
	return ids
}

// dirIdentity returns the identity of the directory at path: its device
// and inode, and its birth time where the file system reports one. ok is
// false when path is not a directory or has no device and inode. Tests
// replace it to simulate inode reuse.
var dirIdentity = func(path string) (id store.DirID, ok bool) {
	fi, err := safefs.Lstat(path)
	if err != nil || !fi.IsDir() {
		return store.DirID{}, false
	}
	dev, ino, ok := safefs.FileID(fi)
	if !ok {
		return store.DirID{}, false
	}
	birth, _ := safefs.FileBirth(fi)
	return store.DirID{Dev: dev, Ino: ino, Birth: birth}, true
}

// isSyncWarning reports whether err only says a written file was not
// synced, which does not undo the write.
func isSyncWarning(err error) bool { return errors.Is(err, patch.ErrNotSynced) }

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

// Revert undoes one change and marks it undone, as Unwire does for a
// single change without force: it refuses, with a *patch.DriftError, when
// the file changed after aggy changed it, and returns an error for a
// change that is not applied, pending, or failed.
//
// Undoing several changes must revert them newest first; use Unwire.
func (m *Manifest) Revert(ctx context.Context, changeID string) error {
	c, err := m.store.GetChange(ctx, changeID)
	if err != nil {
		return err
	}
	u := m.undo(ctx, c, &unwireRun{single: true, w: io.Discard})
	if u.Change.ChangeID == "" {
		return fmt.Errorf("manifest: change %s is not applied, pending, or failed", changeID)
	}
	return u.Err
}
