package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Parent is a row of parents.
type Parent struct {
	ID         string
	Title      string
	Dir        string
	LastVendor string
	CreatedAt  time.Time
	LastActive time.Time
}

// InsertParent adds a parent.
func (s *Store) InsertParent(ctx context.Context, p Parent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO parents (id, title, dir, last_vendor, created_at, last_active)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.Title, p.Dir, p.LastVendor, millis(p.CreatedAt), millis(p.LastActive))
	return err
}

// GetParent returns the parent with id, or ErrNotFound.
func (s *Store) GetParent(ctx context.Context, id string) (Parent, error) {
	var p Parent
	var created, active int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, dir, last_vendor, created_at, last_active FROM parents WHERE id = ?`, id).
		Scan(&p.ID, &p.Title, &p.Dir, &p.LastVendor, &created, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return Parent{}, ErrNotFound
	}
	if err != nil {
		return Parent{}, err
	}
	p.CreatedAt, p.LastActive = fromMillis(created), fromMillis(active)
	return p, nil
}

// Child is a row of children.
type Child struct {
	ID              string
	ParentID        string
	Vendor          string
	VendorSessionID string
	Kind            string
	Model           string
	Cwd             string
	TranscriptPath  string
	Status          string
	CreatedAt       time.Time
	LastActive      time.Time
}

// InsertChild adds a child. The parent must exist. An empty Status means
// "open".
func (s *Store) InsertChild(ctx context.Context, c Child) error {
	if c.Status == "" {
		c.Status = "open"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO children (id, parent_id, vendor, vendor_session_id, kind, model, cwd,
		                       transcript_path, status, created_at, last_active)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ParentID, c.Vendor, c.VendorSessionID, c.Kind, c.Model, c.Cwd,
		c.TranscriptPath, c.Status, millis(c.CreatedAt), millis(c.LastActive))
	return err
}
