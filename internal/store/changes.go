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
	Status      string
	PlannedAt   time.Time
	AppliedAt   time.Time // zero until applied
	UndoneAt    time.Time // zero until undone
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

// InsertChange records a change. An empty Status means pending.
func (s *Store) InsertChange(ctx context.Context, c Change) error {
	if c.Status == "" {
		c.Status = ChangePending
	}
	dirs, err := json.Marshal(nonNil(c.CreatedDirs))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO changes (change_id, run_id, file, hash_before, hash_after, backup_path,
		                      created_file, file_mode, created_dirs, status, planned_at, applied_at, undone_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ChangeID, c.RunID, c.File, c.HashBefore, c.HashAfter, c.BackupPath,
		c.CreatedFile, int64(c.FileMode.Perm()), string(dirs), c.Status,
		millis(c.PlannedAt), nullMillis(c.AppliedAt), nullMillis(c.UndoneAt))
	return err
}

// FinishChange updates a pending change with its outcome: status, hashes,
// backup path, and the undo facts.
func (s *Store) FinishChange(ctx context.Context, c Change) error {
	dirs, err := json.Marshal(nonNil(c.CreatedDirs))
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE changes SET status = ?, hash_before = ?, hash_after = ?, backup_path = ?,
		        created_file = ?, file_mode = ?, created_dirs = ?, applied_at = ?
		 WHERE change_id = ? AND status = 'pending'`,
		c.Status, c.HashBefore, c.HashAfter, c.BackupPath, c.CreatedFile,
		int64(c.FileMode.Perm()), string(dirs), nullMillis(c.AppliedAt), c.ChangeID)
	if err != nil {
		return err
	}
	return oneRow(res, "pending change "+c.ChangeID)
}

// MarkChangeUndone marks an applied change undone.
func (s *Store) MarkChangeUndone(ctx context.Context, changeID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE changes SET status = 'undone', undone_at = ? WHERE change_id = ? AND status = 'applied'`,
		millis(at), changeID)
	if err != nil {
		return err
	}
	return oneRow(res, "applied change "+changeID)
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
		        file_mode, created_dirs, status, planned_at, applied_at, undone_at
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
			&c.BackupPath, &c.CreatedFile, &mode, &dirs, &c.Status, &planned, &applied, &undone); err != nil {
			return nil, err
		}
		c.FileMode = fs.FileMode(mode).Perm()
		if err := json.Unmarshal([]byte(dirs), &c.CreatedDirs); err != nil {
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
// runID is empty, newest first: the order an undo must run in.
func (s *Store) ChangesNewestFirst(ctx context.Context, runID string) ([]Change, error) {
	cs, err := s.Changes(ctx, runID)
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs, err
}
