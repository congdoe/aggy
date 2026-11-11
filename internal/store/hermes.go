package store

import (
	"context"
	"time"
)

// HermesLaunch is a row of hermes_launches: one Hermes session on screen
// (T6933). The row lives while the CLI runs: aggy run deletes it when the CLI
// exits, and a row whose supervisor is gone is deleted when found.
type HermesLaunch struct {
	ID       int64
	ParentID string
	ChildID  string
	Profile  string
	Root     string
	Cwd      string
	// Plan and PlanSum are the decision plan and its SHA-256; both "" with
	// --no-policy.
	Plan, PlanSum string
	// Token is what aggy's hook shows the supervisor's socket: a hook runs in
	// Hermes' daemon, outside the CLI's process tree.
	Token string
	// SessionID is Hermes' session id, "" until a hook named it.
	SessionID string
	Started   time.Time
}

// PutHermesLaunch records a launch and returns its row id.
func (s *Store) PutHermesLaunch(ctx context.Context, l HermesLaunch) (int64, error) {
	r, err := s.db.ExecContext(ctx,
		`INSERT INTO hermes_launches (parent_id, child_id, profile, root, cwd, plan, plan_sum, token, session_id, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ParentID, l.ChildID, l.Profile, l.Root, l.Cwd, l.Plan, l.PlanSum, l.Token, l.SessionID, millis(l.Started))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// DeleteHermesLaunch removes a launch's row.
func (s *Store) DeleteHermesLaunch(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM hermes_launches WHERE id = ?`, id)
	return err
}

// HermesLaunches lists the launches recorded for profile, oldest first.
func (s *Store) HermesLaunches(ctx context.Context, profile string) ([]HermesLaunch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, parent_id, child_id, profile, root, cwd, plan, plan_sum, token, session_id, started_at
		 FROM hermes_launches WHERE profile = ? ORDER BY id`, profile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HermesLaunch
	for rows.Next() {
		var l HermesLaunch
		var at int64
		if err := rows.Scan(&l.ID, &l.ParentID, &l.ChildID, &l.Profile, &l.Root, &l.Cwd, &l.Plan, &l.PlanSum, &l.Token, &l.SessionID, &at); err != nil {
			return nil, err
		}
		l.Started = fromMillis(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ClaimHermesSession sets a launch's session id to id when it holds from
// (its session id as last read), so two hooks naming two new sessions never
// both take one launch. ok is false when the row changed since.
func (s *Store) ClaimHermesSession(ctx context.Context, launch int64, from, id string) (ok bool, err error) {
	r, err := s.db.ExecContext(ctx, `UPDATE hermes_launches SET session_id = ? WHERE id = ? AND session_id = ?`, id, launch, from)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
