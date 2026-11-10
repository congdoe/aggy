package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ReadApproval is a row of read_approvals: the identity of the file an exact
// read pre-approval names, as aggy permissions add recorded it (T6976).
type ReadApproval struct {
	Path     string
	Dev, Ino uint64
	At       time.Time
}

// PutReadApproval records a's identity, replacing any for its path.
func (s *Store) PutReadApproval(ctx context.Context, a ReadApproval) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO read_approvals (path, dev, ino, added_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (path) DO UPDATE SET dev = excluded.dev, ino = excluded.ino, added_at = excluded.added_at`,
		a.Path, int64(a.Dev), int64(a.Ino), millis(a.At)) //nolint:gosec // G115: the bits are kept, and read back as uint64
	return err
}

// ReadApprovalFor is the identity recorded for path; ok is false when there
// is none.
func (s *Store) ReadApprovalFor(ctx context.Context, path string) (dev, ino uint64, ok bool, err error) {
	var d, i int64
	err = s.db.QueryRowContext(ctx, `SELECT dev, ino FROM read_approvals WHERE path = ?`, path).Scan(&d, &i)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return uint64(d), uint64(i), true, nil //nolint:gosec // G115: the bits PutReadApproval stored
}

// DeleteReadApproval forgets path's identity.
func (s *Store) DeleteReadApproval(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM read_approvals WHERE path = ?`, path)
	return err
}
