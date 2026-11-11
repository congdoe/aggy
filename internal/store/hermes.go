package store

import (
	"context"
	"errors"
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
	// Token is what aggy's hook shows the supervisor's socket: a hook runs in
	// Hermes' daemon, outside the CLI's process tree. The decision plan's pin
	// is not here: the hook asks the supervisor for it (#134 review), since a
	// tool the model runs can write this index.
	Token string
	// SessionID is Hermes' session id, "" until a hook named it.
	SessionID string
	Started   time.Time
}

// ErrHermesUnclaimed is a launch refused because another in the same profile
// has no session yet.
var ErrHermesUnclaimed = errors.New("store: another Hermes launch in this profile has no session yet")

// PutHermesLaunch records a launch and returns its row id. A launch with no
// session yet is recorded only while no other row in the profile has none, in
// the one statement (#134 review), so two new sessions are never open to the
// same claim: ErrHermesUnclaimed. The caller removes rows whose supervisor is
// gone first.
func (s *Store) PutHermesLaunch(ctx context.Context, l HermesLaunch) (int64, error) {
	r, err := s.db.ExecContext(ctx,
		`INSERT INTO hermes_launches (parent_id, child_id, profile, root, cwd, token, session_id, started_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE ? != '' OR NOT EXISTS (SELECT 1 FROM hermes_launches WHERE profile = ? AND session_id = '')`,
		l.ParentID, l.ChildID, l.Profile, l.Root, l.Cwd, l.Token, l.SessionID, millis(l.Started), l.SessionID, l.Profile)
	if err != nil {
		return 0, err
	}
	if n, err := r.RowsAffected(); err != nil {
		return 0, err
	} else if n == 0 {
		return 0, ErrHermesUnclaimed
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
		`SELECT id, parent_id, child_id, profile, root, cwd, token, session_id, started_at
		 FROM hermes_launches WHERE profile = ? ORDER BY id`, profile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HermesLaunch
	for rows.Next() {
		var l HermesLaunch
		var at int64
		if err := rows.Scan(&l.ID, &l.ParentID, &l.ChildID, &l.Profile, &l.Root, &l.Cwd, &l.Token, &l.SessionID, &at); err != nil {
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
