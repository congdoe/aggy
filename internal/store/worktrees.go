package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	err := s.db.QueryRowContext(ctx,
		`SELECT c.worktree, c.branch, c.worktree_base, c.worktree_repo, c.worktree_git_dir, c.worktree_state
		 FROM borrows b JOIN children c ON c.id = b.child_id
		 WHERE b.id = ? AND c.worktree <> ''`, id).
		Scan(&w.Path, &w.Branch, &w.Base, &w.Repo, &w.GitDir, &w.State)
	if errors.Is(err, sql.ErrNoRows) {
		return Worktree{}, ErrNotFound
	}
	return w, err
}

// ErrWorktreeState means a worktree was not in the state a change
// expected, such as a second accept.
var ErrWorktreeState = errors.New("store: the worktree is no longer in that state")

// SetWorktreeState moves borrow id's worktree from one state to another,
// and only from it, so two accepts or an accept and a reject cannot both
// win.
func (s *Store) SetWorktreeState(ctx context.Context, id, from, to string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE children SET worktree_state = ?
		 WHERE id = (SELECT child_id FROM borrows WHERE id = ?) AND worktree <> '' AND worktree_state = ?`,
		to, id, from)
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
