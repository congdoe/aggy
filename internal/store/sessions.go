package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const parentCols = `id, title, dir, last_vendor, created_at, last_active`

func scanParent(sc interface{ Scan(...any) error }) (Parent, error) {
	var p Parent
	var created, active int64
	if err := sc.Scan(&p.ID, &p.Title, &p.Dir, &p.LastVendor, &created, &active); err != nil {
		return Parent{}, err
	}
	p.CreatedAt, p.LastActive = fromMillis(created), fromMillis(active)
	return p, nil
}

func (s *Store) queryParents(ctx context.Context, q string, args ...any) ([]Parent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+parentCols+` FROM parents `+q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Parent
	for rows.Next() {
		p, err := scanParent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Parents returns every parent, most recently active first.
func (s *Store) Parents(ctx context.Context) ([]Parent, error) {
	return s.queryParents(ctx, `ORDER BY last_active DESC, created_at DESC, id`)
}

// LatestParentInDir returns the most recently active parent whose
// directory is dir, or ErrNotFound.
func (s *Store) LatestParentInDir(ctx context.Context, dir string) (Parent, error) {
	ps, err := s.queryParents(ctx, `WHERE dir = ? ORDER BY last_active DESC, created_at DESC, id LIMIT 1`, dir)
	if err != nil {
		return Parent{}, err
	}
	if len(ps) == 0 {
		return Parent{}, ErrNotFound
	}
	return ps[0], nil
}

// ParentsByPrefix returns the parents whose id starts with prefix, which
// must hold only hex digits and dashes.
func (s *Store) ParentsByPrefix(ctx context.Context, prefix string) ([]Parent, error) {
	if prefix == "" {
		return nil, fmt.Errorf("store: empty parent id")
	}
	for _, r := range prefix {
		if !strings.ContainsRune("0123456789abcdef-", r) {
			return nil, fmt.Errorf("store: parent id %q is not hex", prefix)
		}
	}
	return s.queryParents(ctx, `WHERE substr(id, 1, ?) = ? ORDER BY id`, len(prefix), prefix)
}

const childCols = `id, parent_id, vendor, vendor_session_id, kind, title, model, cwd,
	transcript_path, status, created_at, last_active`

// Children returns the parent's children, most recently active first.
func (s *Store) Children(ctx context.Context, parentID string) ([]Child, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+childCols+` FROM children WHERE parent_id = ?
		 ORDER BY last_active DESC, created_at DESC, id`, parentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Child
	for rows.Next() {
		var c Child
		var created, active int64
		if err := rows.Scan(&c.ID, &c.ParentID, &c.Vendor, &c.VendorSessionID, &c.Kind, &c.Title, &c.Model,
			&c.Cwd, &c.TranscriptPath, &c.Status, &created, &active); err != nil {
			return nil, err
		}
		c.CreatedAt, c.LastActive = fromMillis(created), fromMillis(active)
		out = append(out, c)
	}
	return out, rows.Err()
}

// OnScreen records that child (of vendor) came on screen at at: the
// child and its parent become active then, and the parent's last vendor
// is vendor.
func (s *Store) OnScreen(ctx context.Context, parentID, childID, vendor string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE children SET last_active = MAX(last_active, ?) WHERE id = ?`, millis(at), childID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE parents SET last_vendor = ?, last_active = MAX(last_active, ?) WHERE id = ?`,
			vendor, millis(at), parentID)
		return err
	})
}

// SetChildActivity records what aggy found about a child: activity at
// active (taken from the vendor's session file; zero leaves it) raises
// the child's and its parent's last-active times, never lowers them, and
// a non-empty status replaces the child's.
func (s *Store) SetChildActivity(ctx context.Context, childID string, active time.Time, status string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if status != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE children SET status = ? WHERE id = ?`, status, childID); err != nil {
				return err
			}
		}
		if active.IsZero() {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE children SET last_active = MAX(last_active, ?) WHERE id = ?`, millis(active), childID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE parents SET last_active = MAX(last_active, ?)
			 WHERE id = (SELECT parent_id FROM children WHERE id = ?)`, millis(active), childID)
		return err
	})
}

// ErrSessionSet means a child already has a different vendor session id.
var ErrSessionSet = errors.New("store: child already has a vendor session id")

// SetChildSessionID records the vendor session id of a child started
// without one (an interactive Codex session, whose id comes later from a
// hook or its sessions folder). Setting the same id again is a no-op; a
// different one is ErrSessionSet, and an id another child of the vendor
// holds fails on the unique index.
func (s *Store) SetChildSessionID(ctx context.Context, childID, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("store: empty vendor session id")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE children SET vendor_session_id = ? WHERE id = ? AND vendor_session_id IN ('', ?)`,
		sessionID, childID, sessionID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	var cur string
	err = s.db.QueryRowContext(ctx, `SELECT vendor_session_id FROM children WHERE id = ?`, childID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: child %s", ErrNotFound, childID)
	}
	if err != nil {
		return err
	}
	return ErrSessionSet
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
