package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
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

// StartBorrow records a new borrow and its child in one transaction. The
// child's parent must exist.
func (s *Store) StartBorrow(ctx context.Context, c Child, b Borrow) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return immediate(ctx, conn, func() error { return insertBorrow(ctx, conn, c, b, 0, "") })
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
		                       transcript_path, status, created_at, last_active)
		 VALUES (?, ?, ?, '', ?, ?, ?, ?, '', ?, ?, ?)`,
		c.ID, c.ParentID, c.Vendor, c.Kind, c.Title, c.Model, c.Cwd, c.Status,
		millis(c.CreatedAt), millis(c.LastActive)); err != nil {
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
