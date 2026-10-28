package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Check gate results on a worktree's recorded tree.
const (
	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckSkipped = "skipped"
)

// Worktree states.
const (
	WorktreePending  = "pending"
	WorktreeAccepted = "accepted"
	WorktreeRejected = "rejected"
)

// BorrowWorktree returns the worktree of borrow id's child, or
// ErrNotFound when the borrow has no child with a worktree.
func (s *Store) BorrowWorktree(ctx context.Context, id string) (Worktree, error) {
	var w Worktree
	var removed sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT c.worktree, c.branch, c.worktree_base, c.worktree_repo, c.worktree_git_dir, c.worktree_state, c.worktree_tree, c.worktree_check,
		        c.worktree_removed_at
		 FROM borrows b JOIN children c ON c.id = b.child_id
		 WHERE b.id = ? AND c.worktree <> ''`, id).
		Scan(&w.Path, &w.Branch, &w.Base, &w.Repo, &w.GitDir, &w.State, &w.Tree, &w.Check, &removed)
	if errors.Is(err, sql.ErrNoRows) {
		return Worktree{}, ErrNotFound
	}
	w.Removed = fromNull(removed)
	return w, err
}

// ErrWorktreeState means a worktree was not in the state a change
// expected, such as a second accept.
var ErrWorktreeState = errors.New("store: the worktree is no longer in that state")

// SetWorktreeState moves borrow id's worktree from one state to another,
// and only from it, so two accepts or an accept and a reject cannot both
// win. A move to accepted records when (schema version 14); any other
// move clears that.
func (s *Store) SetWorktreeState(ctx context.Context, id, from, to string) error {
	var accepted sql.NullInt64
	if to == WorktreeAccepted {
		accepted = sql.NullInt64{Int64: millis(time.Now()), Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE children SET worktree_state = ?, worktree_accepted_at = ?
		 WHERE id = (SELECT child_id FROM borrows WHERE id = ?) AND worktree <> '' AND worktree_state = ?`,
		to, accepted, id, from)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w (borrow %s, want %s)", ErrWorktreeState, id, from)
	}
	return nil
}

// SetWorktreeTree records the tree of borrow id's worktree, once, while
// it is pending.
func (s *Store) SetWorktreeTree(ctx context.Context, id, tree string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE children SET worktree_tree = ?
		 WHERE id = (SELECT child_id FROM borrows WHERE id = ?) AND worktree <> '' AND worktree_tree = '' AND worktree_state = 'pending'`,
		tree, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("%w (borrow %s: tree already set or not pending)", ErrWorktreeState, id)
	}
	return nil
}

// SetWorktreeCheck records the check gate's result on borrow id's
// worktree while it is pending.
func (s *Store) SetWorktreeCheck(ctx context.Context, id, result string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE children SET worktree_check = ?
		 WHERE id = (SELECT child_id FROM borrows WHERE id = ?) AND worktree <> '' AND worktree_state = 'pending'`,
		result, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("%w (borrow %s: not pending)", ErrWorktreeState, id)
	}
	return nil
}

// ClaimWorktreeRemoval marks borrow id's worktree removed as of at, only
// while it is in state and not yet marked, so two removals (aggy reject
// and aggy gc, or two gc runs) never both act on it. A removal that
// fails gives the claim back with UnclaimWorktreeRemoval.
func (s *Store) ClaimWorktreeRemoval(ctx context.Context, id, state string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE children SET worktree_removed_at = ?
		 WHERE id = (SELECT child_id FROM borrows WHERE id = ?) AND worktree <> ''
		   AND worktree_state = ? AND worktree_removed_at IS NULL`,
		millis(at), id, state)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("%w (borrow %s: not %s, or already removed)", ErrWorktreeState, id, state)
	}
	return nil
}

// UnclaimWorktreeRemoval clears the mark ClaimWorktreeRemoval set.
func (s *Store) UnclaimWorktreeRemoval(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(context.WithoutCancel(ctx),
		`UPDATE children SET worktree_removed_at = NULL
		 WHERE id = (SELECT child_id FROM borrows WHERE id = ?) AND worktree <> ''`, id)
	return err
}

// EditRecord is aggy's own record of an edit borrow: the vendor that ran
// it and its worktree, tree included.
type EditRecord struct {
	BorrowID string
	Vendor   string
	Worktree Worktree
}

// EditRecords returns every edit borrow whose worktree had its tree
// recorded, in any state, removed ones included, by borrow id. A review
// matches them against its diff to find which vendors made the change.
func (s *Store) EditRecords(ctx context.Context) ([]EditRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.id, b.vendor, c.worktree, c.branch, c.worktree_base, c.worktree_repo, c.worktree_git_dir,
		        c.worktree_state, c.worktree_tree, c.worktree_removed_at
		 FROM borrows b JOIN children c ON c.id = b.child_id
		 WHERE c.worktree <> '' AND c.worktree_tree <> '' ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EditRecord
	for rows.Next() {
		var r EditRecord
		var removed sql.NullInt64
		w := &r.Worktree
		if err := rows.Scan(&r.BorrowID, &r.Vendor, &w.Path, &w.Branch, &w.Base, &w.Repo, &w.GitDir,
			&w.State, &w.Tree, &removed); err != nil {
			return nil, err
		}
		w.Removed = fromNull(removed)
		out = append(out, r)
	}
	return out, rows.Err()
}
