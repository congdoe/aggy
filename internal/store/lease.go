package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/congdoe/aggy/internal/safefs"
)

// ErrBusy matches, through errors.Is, a *BusyError.
var ErrBusy = errors.New("store: vendor session is held by another process")

// ErrLeaseLost means a lease was taken over before it was released, which
// happens only where file locks are unsupported and the holder's pid
// looked dead.
var ErrLeaseLost = errors.New("store: lease was taken over")

// BusyError reports the live process holding a vendor session.
type BusyError struct {
	Vendor, SessionID string
	PID               int
	Since             time.Time
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("store: %s session %s is held by pid %d since %s: %v",
		e.Vendor, e.SessionID, e.PID, e.Since.Format(time.RFC3339), ErrBusy)
}

// Is reports whether target is ErrBusy.
func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// Lease is this process's hold on one vendor session: only one process
// may drive a vendor session at a time, since two writers on one
// transcript corrupt it.
//
// The children row records the holder's pid and a nonce, and the holder
// keeps an exclusive flock on run/leases/<nonce>.lock in the state
// directory until it releases the lease or exits. The kernel drops the
// lock when the process dies, so another process can tell a stale lease
// from a live one by trying the lock, and a reused pid does not keep a
// dead lease alive. A missing lock file means a dead holder. Only where
// file locks are unsupported does liveness fall back to the pid.
type Lease struct {
	s       *Store
	childID string
	nonce   string
	unlock  func()
	lock    string
}

// ChildID returns the id of the leased children row.
func (l *Lease) ChildID() string { return l.childID }

// leaseDir holds the lease lock files, next to the database.
func (s *Store) leaseDir() string { return filepath.Join(filepath.Dir(s.path), "run", "leases") }

func (s *Store) lockPath(nonce string) string { return filepath.Join(s.leaseDir(), nonce+".lock") }

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type leaseRow struct {
	childID string
	pid     sql.NullInt64
	at      sql.NullInt64
	nonce   sql.NullString
}

func (s *Store) leaseRow(ctx context.Context, vendor, sessionID string) (leaseRow, error) {
	var r leaseRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, lease_pid, lease_at, lease_nonce FROM children
		 WHERE vendor = ? AND vendor_session_id = ? AND vendor_session_id <> ''`, vendor, sessionID).
		Scan(&r.childID, &r.pid, &r.at, &r.nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: %s session %s", ErrNotFound, vendor, sessionID)
	}
	return r, err
}

// alive reports whether the lease held under nonce by pid is still live.
// A live holder always has its lock file: it creates the file before it
// records the lease and removes it only after clearing the record. So a
// missing lock file means the holder is gone, whatever the pid, which
// another process may have reused (#10-11).
func (s *Store) alive(nonce string, pid int) bool {
	held, err := lockHeld(s.lockPath(nonce))
	switch {
	case err == nil:
		return held
	case errors.Is(err, fs.ErrNotExist):
		return false
	}
	// shortcut: where file locks are unsupported the pid is all there is,
	// and a reused pid keeps a dead lease alive; pair it with the process
	// start time if aggy has to support such file systems.
	return pidAlive(pid)
}

// AcquireLease takes the lease on the vendor session, whose children row
// must exist. A live holder makes it fail with a *BusyError; a stale lease
// (its holder exited) is reclaimed.
func (s *Store) AcquireLease(ctx context.Context, vendor, sessionID string) (*Lease, error) {
	if err := safefs.MkdirAll(s.leaseDir(), 0o700); err != nil {
		return nil, err
	}
	for range 5 {
		r, err := s.leaseRow(ctx, vendor, sessionID)
		if err != nil {
			return nil, err
		}
		if r.nonce.Valid && s.alive(r.nonce.String, int(r.pid.Int64)) {
			return nil, &BusyError{Vendor: vendor, SessionID: sessionID, PID: int(r.pid.Int64), Since: fromMillis(r.at.Int64)}
		}
		nonce, err := newNonce()
		if err != nil {
			return nil, err
		}
		lock := s.lockPath(nonce)
		unlock, err := takeLock(lock)
		if err != nil {
			return nil, err
		}
		// Compare and swap on the nonce seen above, so of two processes
		// reclaiming the same stale lease only one wins.
		res, err := s.db.ExecContext(ctx,
			`UPDATE children SET lease_pid = ?, lease_nonce = ?, lease_at = ?
			 WHERE id = ? AND lease_nonce IS ?`,
			os.Getpid(), nonce, millis(time.Now()), r.childID, r.nonce)
		var n int64
		if err == nil {
			n, err = res.RowsAffected()
		}
		if err != nil || n != 1 {
			unlock()
			_ = safefs.Remove(lock)
			if err != nil {
				return nil, err
			}
			continue // lost the race; look again
		}
		if r.nonce.Valid {
			// The stale holder's lock file.
			_ = safefs.Remove(s.lockPath(r.nonce.String))
		}
		return &Lease{s: s, childID: r.childID, nonce: nonce, unlock: unlock, lock: lock}, nil
	}
	return nil, fmt.Errorf("%w: %s session %s kept changing hands", ErrBusy, vendor, sessionID)
}

// Release gives up the lease. Releasing twice is a no-op.
func (l *Lease) Release(ctx context.Context) error {
	if l.unlock == nil {
		return nil
	}
	res, err := l.s.db.ExecContext(context.WithoutCancel(ctx),
		`UPDATE children SET lease_pid = NULL, lease_nonce = NULL, lease_at = NULL
		 WHERE id = ? AND lease_nonce = ?`, l.childID, l.nonce)
	var n int64
	if err == nil {
		n, err = res.RowsAffected()
	}
	l.unlock()
	l.unlock = nil
	if rerr := safefs.Remove(l.lock); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && err == nil {
		err = rerr
	}
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLeaseLost
	}
	return nil
}

// LeaseHolder returns the pid holding the vendor session's lease, and
// whether that holder is alive. A stale lease reports its pid and false.
func (s *Store) LeaseHolder(ctx context.Context, vendor, sessionID string) (pid int, alive bool, err error) {
	r, err := s.leaseRow(ctx, vendor, sessionID)
	if err != nil || !r.nonce.Valid {
		return 0, false, err
	}
	return int(r.pid.Int64), s.alive(r.nonce.String, int(r.pid.Int64)), nil
}
