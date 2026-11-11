package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ReadApproval is a row of read_approvals: the identity of the file an exact
// read pre-approval names, as aggy permissions add recorded it (T6976).
// parity.ReadApproval says what each field defends against.
type ReadApproval struct {
	Path     string
	Dev, Ino uint64
	Size     int64
	ModNS    int64 // modification time, Unix nanoseconds
	ChangeNS int64 // change time (ctime), Unix nanoseconds; 0 where the OS has none
	At       time.Time
	// Complete is false for a row written before aggy recorded the size and
	// times (migration read_approval_stamps): it must match no file.
	Complete bool
}

// PutReadApproval records a's identity, replacing any for its path. The row
// it writes is complete whatever a.Complete says.
func (s *Store) PutReadApproval(ctx context.Context, a ReadApproval) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO read_approvals (path, dev, ino, size, mod_ns, change_ns, added_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (path) DO UPDATE SET dev = excluded.dev, ino = excluded.ino, size = excluded.size,
		   mod_ns = excluded.mod_ns, change_ns = excluded.change_ns, added_at = excluded.added_at`,
		a.Path, int64(a.Dev), int64(a.Ino), a.Size, a.ModNS, a.ChangeNS, millis(a.At)) //nolint:gosec // G115: the bits are kept, and read back as uint64
	return err
}

// ReadApprovalFor is the identity recorded for path; ok is false when there
// is none. A row from before migration read_approval_stamps comes back with Complete false.
func (s *Store) ReadApprovalFor(ctx context.Context, path string) (a ReadApproval, ok bool, err error) {
	var d, i, at int64
	var size, mod, change sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT dev, ino, size, mod_ns, change_ns, added_at FROM read_approvals WHERE path = ?`, path).
		Scan(&d, &i, &size, &mod, &change, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return ReadApproval{}, false, nil
	}
	if err != nil {
		return ReadApproval{}, false, err
	}
	return ReadApproval{
		Path: path,
		Dev:  uint64(d), Ino: uint64(i), //nolint:gosec // G115: the bits PutReadApproval stored
		Size: size.Int64, ModNS: mod.Int64, ChangeNS: change.Int64,
		At:       time.UnixMilli(at),
		Complete: size.Valid && mod.Valid && change.Valid,
	}, true, nil
}

// DeleteReadApproval forgets path's identity.
func (s *Store) DeleteReadApproval(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM read_approvals WHERE path = ?`, path)
	return err
}
