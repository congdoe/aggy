package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/congdoe/aggy/internal/safefs"
)

// What aggy gc reads and prunes in the index. The rows only name
// candidates; the caller decides by age and checks each one it removes
// again, and every delete here compares the lease it was shown.

// gcSchema is the version that records worktree removals.
const gcSchema = 14

// removedExpr is c.worktree_removed_at, or, on an index from before
// version 14 that a read-only caller (aggy gc --dry-run) cannot migrate,
// what the migration will set: reject removed its worktrees already.
func (s *Store) removedExpr(ctx context.Context) (string, error) {
	v, err := s.Version(ctx)
	if err != nil {
		return "", err
	}
	if v < gcSchema {
		return `(CASE WHEN c.worktree_state = 'rejected' THEN c.last_active END)`, nil
	}
	return `c.worktree_removed_at`, nil
}

// GCWorktree is an edit worktree that has not been removed.
type GCWorktree struct {
	BorrowID string
	Worktree Worktree
	Running  bool      // its borrow is still marked running
	Held     bool      // its child's lease is held by a live process
	Ended    time.Time // when the borrow ended, or started if it never did
}

// GCWorktrees returns the edit worktrees not yet removed, by borrow id.
func (s *Store) GCWorktrees(ctx context.Context) ([]GCWorktree, error) {
	removed, err := s.removedExpr(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.id, c.worktree, c.branch, c.worktree_base, c.worktree_repo, c.worktree_git_dir,
		        c.worktree_state, c.worktree_tree, b.status, COALESCE(b.ended_at, b.started_at),
		        c.lease_pid, c.lease_nonce
		 FROM children c JOIN borrows b ON b.child_id = c.id
		 WHERE c.worktree <> '' AND `+removed+` IS NULL ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []GCWorktree
	for rows.Next() {
		var g GCWorktree
		var status string
		var ended int64
		var pid sql.NullInt64
		var nonce sql.NullString
		w := &g.Worktree
		if err := rows.Scan(&g.BorrowID, &w.Path, &w.Branch, &w.Base, &w.Repo, &w.GitDir,
			&w.State, &w.Tree, &status, &ended, &pid, &nonce); err != nil {
			return nil, err
		}
		g.Running = status == "running"
		g.Held = nonce.Valid && s.alive(nonce.String, int(pid.Int64))
		g.Ended = fromMillis(ended)
		out = append(out, g)
	}
	return out, rows.Err()
}

// GCChild is a closed borrow child old enough to prune from the index.
type GCChild struct {
	ID, Kind, Vendor string
	LastActive       time.Time
	Held             bool // its lease is held by a live process
	pid              int
	nonce            sql.NullString
}

// pruneWhere is what makes a child prunable: a closed borrow child (an
// interactive one is a session the person can resume), with no worktree
// left, and no borrow still running on it.
const pruneWhere = `c.status = 'closed' AND c.kind <> ''
	AND (c.worktree = '' OR %s IS NOT NULL)
	AND NOT EXISTS (SELECT 1 FROM borrows r WHERE r.child_id = c.id AND r.status = 'running')`

// GCChildren returns the prunable children last active before before.
func (s *Store) GCChildren(ctx context.Context, before time.Time) ([]GCChild, error) {
	removed, err := s.removedExpr(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.kind, c.vendor, c.last_active, c.lease_pid, c.lease_nonce FROM children c
		 WHERE c.last_active < ? AND `+fmt.Sprintf(pruneWhere, removed)+` ORDER BY c.last_active, c.id`,
		millis(before))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []GCChild
	for rows.Next() {
		var c GCChild
		var active int64
		var pid sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Kind, &c.Vendor, &active, &pid, &c.nonce); err != nil {
			return nil, err
		}
		c.LastActive, c.pid = fromMillis(active), int(pid.Int64)
		c.Held = c.nonce.Valid && s.alive(c.nonce.String, int(pid.Int64))
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneChild deletes child c from the index, only while it is still
// prunable and its lease is the one GCChildren saw, held by no live
// process. Its borrows stay, for stats, with no child. Nothing outside
// the index is touched: the vendor's own session files are the vendor's.
func (s *Store) PruneChild(ctx context.Context, c GCChild) (bool, error) {
	if c.nonce.Valid && s.alive(c.nonce.String, c.pid) {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM children AS c WHERE c.id = ? AND c.lease_nonce IS ? AND `+fmt.Sprintf(pruneWhere, "c.worktree_removed_at"),
		c.ID, c.nonce)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if c.nonce.Valid {
		_ = safefs.Remove(s.lockPath(c.nonce.String)) // the dead holder's lock file
	}
	return true, nil
}

// StrayLease is a lease whose holder is gone (its lock is free) while a
// process with the recorded pid is alive. A lease records aggy's own pid
// and no start time, so that process cannot be shown to be the holder:
// the pid is most likely reused.
type StrayLease struct {
	Table, ID string // "child" or "borrow", and the row's id
	PID       int
}

// StrayLeases returns the stale leases on children and running borrows
// whose pid names a live process.
func (s *Store) StrayLeases(ctx context.Context) ([]StrayLease, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT 'child', id, lease_pid, lease_nonce FROM children WHERE lease_nonce IS NOT NULL
		 UNION ALL
		 SELECT 'borrow', id, lease_pid, lease_nonce FROM borrows WHERE lease_nonce IS NOT NULL
		 ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StrayLease
	for rows.Next() {
		var l StrayLease
		var pid sql.NullInt64
		var nonce string
		if err := rows.Scan(&l.Table, &l.ID, &pid, &nonce); err != nil {
			return nil, err
		}
		l.PID = int(pid.Int64)
		if !s.alive(nonce, l.PID) && pidAlive(l.PID) {
			out = append(out, l)
		}
	}
	return out, rows.Err()
}

// ClearReport forgets borrow id's report file, once aggy gc removed it,
// so aggy inbox no longer lists it.
func (s *Store) ClearReport(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE borrows SET report_path = '' WHERE id = ?`, id)
	return err
}
