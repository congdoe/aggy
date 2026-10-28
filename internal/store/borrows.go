package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/safefs"
)

// ParentForDir returns the most recently active parent whose directory is
// dir, creating one titled title when there is none. Until the supervisor
// (M2) opens parents itself, a borrow run from a shell hangs its child on
// the parent of the directory it ran in.
func (s *Store) ParentForDir(ctx context.Context, dir, title, newID string, now time.Time) (Parent, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM parents WHERE dir = ? ORDER BY last_active DESC, id LIMIT 1`, dir).Scan(&id)
	switch {
	case err == nil:
		if _, err := s.db.ExecContext(ctx, `UPDATE parents SET last_active = ? WHERE id = ?`, millis(now), id); err != nil {
			return Parent{}, err
		}
		return s.GetParent(ctx, id)
	case !errors.Is(err, sql.ErrNoRows):
		return Parent{}, err
	}
	p := Parent{ID: newID, Title: title, Dir: dir, CreatedAt: now, LastActive: now}
	if err := s.InsertParent(ctx, p); err != nil {
		return Parent{}, err
	}
	return p, nil
}

// Borrow is a row of borrows.
type Borrow struct {
	ID         string
	ChildID    string
	Kind       string
	Vendor     string
	Model      string
	TaskHash   string
	Budget     BorrowBudget
	Status     string // running, done, failed, or killed
	Outcome    string // "ok" or an adapter error class
	Artifacts  []string
	ImageCount int
	Tokens     int64
	ReportPath string
	StartedAt  time.Time
	EndedAt    time.Time // zero while running
	ReadAt     time.Time // zero while the report is unread
	Title      string    // the child's title, read back with the row
}

// BorrowBudget is a borrow's limits as recorded in its budget column.
type BorrowBudget struct {
	Minutes int `json:"minutes"`
	Turns   int `json:"turns,omitempty"`
}

// BorrowEnd is how a borrow finished.
type BorrowEnd struct {
	Status          string // done, failed, or killed
	Outcome         string
	Artifacts       []string
	ImageCount      int
	Tokens          int64
	ReportPath      string
	VendorSessionID string // recorded on the child; "" leaves it unset
	EndedAt         time.Time
}

const borrowCols = `b.id, COALESCE(b.child_id, ''), b.kind, b.vendor, b.model, b.task_hash, b.budget,
	b.status, b.outcome, b.artifacts, b.image_count, b.tokens, b.report_path, b.started_at,
	b.ended_at, b.read_at, COALESCE(c.title, '')`

func scanBorrow(sc interface{ Scan(...any) error }) (Borrow, error) {
	var b Borrow
	var budget, artifacts string
	var started int64
	var ended, read sql.NullInt64
	if err := sc.Scan(&b.ID, &b.ChildID, &b.Kind, &b.Vendor, &b.Model, &b.TaskHash, &budget,
		&b.Status, &b.Outcome, &artifacts, &b.ImageCount, &b.Tokens, &b.ReportPath, &started,
		&ended, &read, &b.Title); err != nil {
		return Borrow{}, err
	}
	_ = json.Unmarshal([]byte(budget), &b.Budget)
	_ = json.Unmarshal([]byte(artifacts), &b.Artifacts)
	b.StartedAt = fromMillis(started)
	b.EndedAt = fromNull(ended)
	b.ReadAt = fromNull(read)
	return b, nil
}

// GetBorrow returns the borrow with id, or ErrNotFound.
func (s *Store) GetBorrow(ctx context.Context, id string) (Borrow, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+borrowCols+` FROM borrows b LEFT JOIN children c ON c.id = b.child_id WHERE b.id = ?`, id)
	b, err := scanBorrow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Borrow{}, ErrNotFound
	}
	return b, err
}

// BorrowCaps bound the borrows running at once, across every aggy
// process.
type BorrowCaps struct {
	PerVendor int
	Total     int
}

// CapError says a borrow was refused because too many are running.
type CapError struct {
	Vendor        string
	VendorRunning int // running now on Vendor
	TotalRunning  int // running now in all
	Caps          BorrowCaps
}

func (e *CapError) Error() string {
	if e.Caps.PerVendor > 0 && e.VendorRunning >= e.Caps.PerVendor {
		return fmt.Sprintf("%d %s borrows are already running (at most %d per vendor); wait for one to finish",
			e.VendorRunning, e.Vendor, e.Caps.PerVendor)
	}
	return fmt.Sprintf("%d borrows are already running (at most %d in all); wait for one to finish", e.TotalRunning, e.Caps.Total)
}

// ErrCap matches a *CapError.
var ErrCap = errors.New("store: too many borrows running")

// Is reports whether target is ErrCap.
func (e *CapError) Is(target error) bool { return target == ErrCap }

// BorrowLease is this process's hold on a running borrow. It counts
// against the caps until Finish, or until the process dies: the holder
// keeps an flock on run/leases/<nonce>.lock, which the kernel drops with
// the process, so a crashed borrow stops counting.
type BorrowLease struct {
	s      *Store
	id     string
	unlock func()
	lock   string
}

// ID returns the borrow's id.
func (l *BorrowLease) ID() string { return l.id }

// StartBorrow records a new borrow and its child in one write
// transaction, after counting the borrows running now. A running borrow
// whose holder is dead is marked killed (outcome "abandoned") and not
// counted. When caps would be exceeded nothing is recorded and the error
// is a *CapError. The child's parent must exist.
func (s *Store) StartBorrow(ctx context.Context, c Child, b Borrow, caps BorrowCaps) (*BorrowLease, error) {
	if err := safefs.MkdirAll(s.leaseDir(), 0o700); err != nil {
		return nil, err
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
	conn, err := s.db.Conn(ctx)
	if err != nil {
		unlock()
		_ = safefs.Remove(lock)
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	err = immediate(ctx, conn, func() error {
		vendorN, total, err := s.countRunning(ctx, conn, b.Vendor, b.StartedAt)
		if err != nil {
			return err
		}
		if (caps.PerVendor > 0 && vendorN >= caps.PerVendor) || (caps.Total > 0 && total >= caps.Total) {
			return &CapError{Vendor: b.Vendor, VendorRunning: vendorN, TotalRunning: total, Caps: caps}
		}
		return insertBorrow(ctx, conn, c, b, os.Getpid(), nonce)
	})
	if err != nil {
		unlock()
		_ = safefs.Remove(lock)
		return nil, err
	}
	return &BorrowLease{s: s, id: b.ID, unlock: unlock, lock: lock}, nil
}

// countRunning counts live running borrows, on vendor and in all, and
// marks the dead ones killed.
func (s *Store) countRunning(ctx context.Context, conn *sql.Conn, vendor string, now time.Time) (vendorN, total int, err error) {
	rows, err := conn.QueryContext(ctx, `SELECT id, vendor, lease_pid, lease_nonce FROM borrows WHERE status = 'running'`)
	if err != nil {
		return 0, 0, err
	}
	type run struct {
		id, vendor string
		pid        sql.NullInt64
		nonce      sql.NullString
	}
	var runs []run
	for rows.Next() {
		var r run
		if err := rows.Scan(&r.id, &r.vendor, &r.pid, &r.nonce); err != nil {
			_ = rows.Close()
			return 0, 0, err
		}
		runs = append(runs, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, r := range runs {
		live := r.nonce.Valid && s.alive(r.nonce.String, int(r.pid.Int64))
		if !live {
			if _, err := conn.ExecContext(ctx,
				`UPDATE borrows SET status = 'killed', outcome = 'abandoned', ended_at = ?,
				        lease_pid = NULL, lease_nonce = NULL WHERE id = ?`, millis(now), r.id); err != nil {
				return 0, 0, err
			}
			// Its child is no longer running either.
			if _, err := conn.ExecContext(ctx,
				`UPDATE children SET status = 'closed', last_active = ?
				 WHERE id = (SELECT child_id FROM borrows WHERE id = ?) AND status = 'running'`, millis(now), r.id); err != nil {
				return 0, 0, err
			}
			if r.nonce.Valid {
				_ = safefs.Remove(s.lockPath(r.nonce.String))
			}
			continue
		}
		total++
		if r.vendor == vendor {
			vendorN++
		}
	}
	return vendorN, total, nil
}

// Finish records how the borrow ended and gives up the lease. Calling it
// twice is an error from the second update only.
func (l *BorrowLease) Finish(ctx context.Context, e BorrowEnd) error {
	err := l.s.FinishBorrow(ctx, l.id, e)
	if l.unlock != nil {
		l.unlock()
		l.unlock = nil
		_ = safefs.Remove(l.lock)
	}
	return err
}

// immediate runs fn in a BEGIN IMMEDIATE transaction on conn, so another
// writer waits (busy_timeout) instead of interleaving.
func immediate(ctx context.Context, conn *sql.Conn, fn func() error) (err error) {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if err := fn(); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func insertBorrow(ctx context.Context, conn *sql.Conn, c Child, b Borrow, pid int, nonce string) error {
	if c.Status == "" {
		c.Status = "running"
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO children (id, parent_id, vendor, vendor_session_id, kind, title, model, cwd,
		                       transcript_path, status, created_at, last_active,
		                       worktree, branch, worktree_base, worktree_repo, worktree_git_dir, worktree_state)
		 VALUES (?, ?, ?, '', ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ParentID, c.Vendor, c.Kind, c.Title, c.Model, c.Cwd, c.Status,
		millis(c.CreatedAt), millis(c.LastActive),
		c.Worktree.Path, c.Worktree.Branch, c.Worktree.Base, c.Worktree.Repo, c.Worktree.GitDir, c.Worktree.State); err != nil {
		return err
	}
	budget, err := json.Marshal(b.Budget)
	if err != nil {
		return err
	}
	var leasePID sql.NullInt64
	var leaseNonce sql.NullString
	if nonce != "" {
		leasePID = sql.NullInt64{Int64: int64(pid), Valid: true}
		leaseNonce = sql.NullString{String: nonce, Valid: true}
	}
	_, err = conn.ExecContext(ctx,
		`INSERT INTO borrows (id, child_id, kind, vendor, model, task_hash, budget, status, started_at,
		                      lease_pid, lease_nonce)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?)`,
		b.ID, c.ID, b.Kind, b.Vendor, b.Model, b.TaskHash, string(budget), millis(b.StartedAt),
		leasePID, leaseNonce)
	return err
}

// FinishBorrow records how the borrow id ended, clears its lease, closes
// its child, and records the child's vendor session id when one is given.
// A session id another child already holds is left off this child.
func (s *Store) FinishBorrow(ctx context.Context, id string, e BorrowEnd) error {
	arts, err := json.Marshal(nonNil(e.Artifacts))
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	res, err := s.db.ExecContext(ctx,
		`UPDATE borrows SET status = ?, outcome = ?, artifacts = ?, image_count = ?, tokens = ?,
		        report_path = ?, ended_at = ?, lease_pid = NULL, lease_nonce = NULL
		 WHERE id = ?`,
		e.Status, e.Outcome, string(arts), e.ImageCount, e.Tokens, e.ReportPath, millis(e.EndedAt), id)
	if err != nil {
		return err
	}
	if err := oneRow(res, "borrow "+id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE children SET status = 'closed', last_active = ?
		 WHERE id = (SELECT child_id FROM borrows WHERE id = ?)`, millis(e.EndedAt), id); err != nil {
		return err
	}
	if e.VendorSessionID != "" {
		// The unique index on (vendor, vendor_session_id) refuses a
		// session another child holds; the borrow row keeps the result.
		_, _ = s.db.ExecContext(ctx,
			`UPDATE children SET vendor_session_id = ?
			 WHERE id = (SELECT child_id FROM borrows WHERE id = ?)`, e.VendorSessionID, id)
	}
	return nil
}

// InboxBorrows returns the borrows that have a report, newest first:
// only unread ones unless all is set.
func (s *Store) InboxBorrows(ctx context.Context, all bool) ([]Borrow, error) {
	q := `SELECT ` + borrowCols + ` FROM borrows b LEFT JOIN children c ON c.id = b.child_id
	      WHERE b.report_path <> ''`
	if !all {
		q += ` AND b.read_at IS NULL`
	}
	q += ` ORDER BY b.started_at DESC, b.id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Borrow
	for rows.Next() {
		b, err := scanBorrow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BorrowsByPrefix returns the borrows whose id starts with prefix.
// prefix must be hex digits only.
func (s *Store) BorrowsByPrefix(ctx context.Context, prefix string) ([]Borrow, error) {
	for _, r := range prefix {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return nil, fmt.Errorf("store: borrow id %q is not hex", prefix)
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+borrowCols+` FROM borrows b LEFT JOIN children c ON c.id = b.child_id
		 WHERE substr(b.id, 1, ?) = ? ORDER BY b.id`, len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Borrow
	for rows.Next() {
		b, err := scanBorrow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkBorrowRead marks borrow id's report read, if it is not already.
func (s *Store) MarkBorrowRead(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE borrows SET read_at = ? WHERE id = ? AND read_at IS NULL`, millis(at), id)
	return err
}

// BorrowUsage is what aggy itself saw one vendor's finished borrows use:
// the CLI's own token counts and wall time. It is never the vendor's
// count, bill, or allowance.
type BorrowUsage struct {
	Vendor string
	// Borrows is the number of finished borrows; WithTokens, how many of
	// them reported tokens (a CLI that reports none records 0).
	Borrows, WithTokens int
	Tokens              int64
	Time                time.Duration
}

// BorrowUsageSince sums the borrows that started at or after since and
// have finished, per vendor, by vendor name.
func (s *Store) BorrowUsageSince(ctx context.Context, since time.Time) ([]BorrowUsage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT vendor, COUNT(*), SUM(CASE WHEN tokens > 0 THEN 1 ELSE 0 END),
		        SUM(MAX(tokens, 0)), SUM(MAX(ended_at - started_at, 0))
		 FROM borrows WHERE started_at >= ? AND ended_at IS NOT NULL
		 GROUP BY vendor ORDER BY vendor`, millis(since))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BorrowUsage
	for rows.Next() {
		var u BorrowUsage
		var ms int64
		if err := rows.Scan(&u.Vendor, &u.Borrows, &u.WithTokens, &u.Tokens, &ms); err != nil {
			return nil, err
		}
		u.Time = time.Duration(ms) * time.Millisecond
		out = append(out, u)
	}
	return out, rows.Err()
}
