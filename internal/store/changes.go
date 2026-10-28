package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"time"
)

// Change statuses. A change is recorded as pending before its file is
// written, then marked applied, noop, or failed.
const (
	ChangePending = "pending"
	ChangeApplied = "applied"
	ChangeNoop    = "noop"
	ChangeFailed  = "failed"
	ChangeUndone  = "undone"
)

// Change kinds. A marker row records that a write run happened and
// changed nothing; it names no file and has nothing to undo.
const (
	KindChange = "change"
	KindMarker = "marker"
)

// Change is a row of changes: one config-file change and what undoing it
// exactly needs.
type Change struct {
	ID          int64
	ChangeID    string // names the backup folder, backups/<ChangeID>
	RunID       string // the wire run or group the change belongs to
	File        string
	HashBefore  string // "none" when the change creates the file
	HashAfter   string
	BackupPath  string
	CreatedFile bool
	FileMode    fs.FileMode // permission bits before the change, or of the created file
	CreatedDirs []string    // directories made for a new file, outermost first
	// CreatedDirIDs holds each created directory's identity, by index
	// into CreatedDirs, recorded once the directory exists. A zero DirID
	// means unknown (a pending row, a row from before the identities were
	// recorded, or a directory aggy has since seen removed). Nil means
	// none is known.
	CreatedDirIDs []DirID
	Status        string
	Kind          string // KindChange or KindMarker; empty means KindChange
	PlannedAt     time.Time
	AppliedAt     time.Time // zero until applied
	UndoneAt      time.Time // zero until undone
}

// DirID is a directory's device and inode.
type DirID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// createdDir is one element of created_dirs in the JSON object form.
// Older rows hold a plain list of paths; both forms are read.
type createdDir struct {
	Path string `json:"path"`
	Dev  uint64 `json:"dev,omitempty"`
	Ino  uint64 `json:"ino,omitempty"`
}

// encodeDirs writes created_dirs as a JSON list of {path, dev, ino}.
func encodeDirs(dirs []string, ids []DirID) (string, error) {
	out := make([]createdDir, len(dirs))
	for i, d := range dirs {
		out[i].Path = d
		if i < len(ids) {
			out[i].Dev, out[i].Ino = ids[i].Dev, ids[i].Ino
		}
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// decodeDirs reads created_dirs in either form: a list of paths, or a
// list of {path, dev, ino}.
func decodeDirs(text string) ([]string, []DirID, error) {
	var paths []string
	if err := json.Unmarshal([]byte(text), &paths); err == nil {
		return nonNil(paths), nil, nil
	}
	var objs []createdDir
	if err := json.Unmarshal([]byte(text), &objs); err != nil {
		return nil, nil, err
	}
	paths = make([]string, len(objs))
	var ids []DirID
	for i, o := range objs {
		paths[i] = o.Path
		if o.Dev != 0 || o.Ino != 0 {
			if ids == nil {
				ids = make([]DirID, len(objs))
			}
			ids[i] = DirID{Dev: o.Dev, Ino: o.Ino}
		}
	}
	return paths, ids, nil
}

// SetCreatedDirIDs replaces the recorded identities of a change's
// created directories.
func (s *Store) SetCreatedDirIDs(ctx context.Context, changeID string, dirs []string, ids []DirID) error {
	text, err := encodeDirs(dirs, ids)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE changes SET created_dirs = ? WHERE change_id = ?`, text, changeID)
	if err != nil {
		return err
	}
	return oneRow(res, "change "+changeID)
}

func nullMillis(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: millis(t), Valid: true}
}

func fromNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return fromMillis(n.Int64)
}

// InsertChange records a change. An empty Status means pending, and an
// empty Kind means KindChange.
func (s *Store) InsertChange(ctx context.Context, c Change) error {
	if c.Status == "" {
		c.Status = ChangePending
	}
	if c.Kind == "" {
		c.Kind = KindChange
	}
	dirs, err := encodeDirs(c.CreatedDirs, c.CreatedDirIDs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO changes (change_id, run_id, file, hash_before, hash_after, backup_path,
		                      created_file, file_mode, created_dirs, status, kind, planned_at, applied_at, undone_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ChangeID, c.RunID, c.File, c.HashBefore, c.HashAfter, c.BackupPath,
		c.CreatedFile, int64(c.FileMode.Perm()), dirs, c.Status, c.Kind,
		millis(c.PlannedAt), nullMillis(c.AppliedAt), nullMillis(c.UndoneAt))
	return err
}

// FinishChange updates a pending change with its outcome: status, hashes,
// backup path, and the undo facts.
func (s *Store) FinishChange(ctx context.Context, c Change) error {
	dirs, err := encodeDirs(c.CreatedDirs, c.CreatedDirIDs)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE changes SET status = ?, hash_before = ?, hash_after = ?, backup_path = ?,
		        created_file = ?, file_mode = ?, created_dirs = ?, applied_at = ?
		 WHERE change_id = ? AND status = 'pending'`,
		c.Status, c.HashBefore, c.HashAfter, c.BackupPath, c.CreatedFile,
		int64(c.FileMode.Perm()), dirs, nullMillis(c.AppliedAt), c.ChangeID)
	if err != nil {
		return err
	}
	return oneRow(res, "pending change "+c.ChangeID)
}

// MarkChangeUndone marks a change undone. Only an applied, pending, or
// failed change can be undone: aggy unwire undoes a pending or failed
// change too, whose write may have happened before the process stopped.
func (s *Store) MarkChangeUndone(ctx context.Context, changeID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE changes SET status = 'undone', undone_at = ?
		 WHERE change_id = ? AND status IN ('applied', 'pending', 'failed') AND kind = 'change'`,
		millis(at), changeID)
	if err != nil {
		return err
	}
	return oneRow(res, "undoable change "+changeID)
}

func oneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return nil
}

// GetChange returns the change with changeID, or ErrNotFound.
func (s *Store) GetChange(ctx context.Context, changeID string) (Change, error) {
	cs, err := s.queryChanges(ctx, `WHERE change_id = ?`, changeID)
	if err != nil {
		return Change{}, err
	}
	if len(cs) == 0 {
		return Change{}, ErrNotFound
	}
	return cs[0], nil
}

// Changes returns the changes of one run, or of every run when runID is
// empty, oldest first.
func (s *Store) Changes(ctx context.Context, runID string) ([]Change, error) {
	if runID == "" {
		return s.queryChanges(ctx, ``)
	}
	return s.queryChanges(ctx, `WHERE run_id = ?`, runID)
}

func (s *Store) queryChanges(ctx context.Context, where string, args ...any) ([]Change, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, change_id, run_id, file, hash_before, hash_after, backup_path, created_file,
		        file_mode, created_dirs, status, kind, planned_at, applied_at, undone_at
		 FROM changes `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Change
	for rows.Next() {
		var c Change
		var mode int64
		var dirs string
		var planned int64
		var applied, undone sql.NullInt64
		if err := rows.Scan(&c.ID, &c.ChangeID, &c.RunID, &c.File, &c.HashBefore, &c.HashAfter,
			&c.BackupPath, &c.CreatedFile, &mode, &dirs, &c.Status, &c.Kind, &planned, &applied, &undone); err != nil {
			return nil, err
		}
		c.FileMode = fs.FileMode(mode).Perm()
		if c.CreatedDirs, c.CreatedDirIDs, err = decodeDirs(dirs); err != nil {
			return nil, fmt.Errorf("store: change %s: created_dirs: %w", c.ChangeID, err)
		}
		c.PlannedAt, c.AppliedAt, c.UndoneAt = fromMillis(planned), fromNull(applied), fromNull(undone)
		out = append(out, c)
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ChangesNewestFirst returns the changes of one run, or of every run when
// runID is empty, newest first: the order an undo must run in. Marker rows
// are left out; they name no file and have nothing to undo.
func (s *Store) ChangesNewestFirst(ctx context.Context, runID string) ([]Change, error) {
	where, args := `WHERE kind <> 'marker'`, []any{}
	if runID != "" {
		where, args = where+` AND run_id = ?`, append(args, runID)
	}
	cs, err := s.queryChanges(ctx, where, args...)
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs, err
}

// UndoableAfter returns the change rows newer than id (by row id) that
// are applied, pending, or failed, oldest first. aggy unwire uses it to
// find a newer change to the same file before it undoes an older one.
func (s *Store) UndoableAfter(ctx context.Context, id int64) ([]Change, error) {
	return s.queryChanges(ctx, `WHERE id > ? AND kind = 'change' AND status IN ('applied', 'pending', 'failed')`, id)
}
