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
	// DirKey is Dir's canonical form, which parents are matched by; DirID
	// is Dir's device and inode when the parent was made. Both may be
	// empty (rows from before schema version 10, borrow parents).
	DirKey string
	DirID  string
}

// InsertParent adds a parent.
func (s *Store) InsertParent(ctx context.Context, p Parent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO parents (id, title, dir, last_vendor, created_at, last_active, dir_key, dir_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Title, p.Dir, p.LastVendor, millis(p.CreatedAt), millis(p.LastActive), p.DirKey, p.DirID)
	return err
}

// GetParent returns the parent with id, or ErrNotFound.
func (s *Store) GetParent(ctx context.Context, id string) (Parent, error) {
	p, err := scanParent(s.db.QueryRowContext(ctx, `SELECT `+parentCols+` FROM parents WHERE id = ?`, capMillis(), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Parent{}, ErrNotFound
	}
	return p, err
}

// Child is a row of children.
type Child struct {
	ID              string
	ParentID        string
	Vendor          string
	VendorSessionID string
	Kind            string
	Title           string
	Model           string
	Cwd             string
	TranscriptPath  string
	// VendorHome is the vendor's home folder at launch, where its session
	// files are looked for; empty means the current environment's.
	VendorHome string
	// FileSeen is set once the child's session file has been found on
	// disk (schema version 11).
	FileSeen   bool
	Status     string
	CreatedAt  time.Time
	LastActive time.Time
	// Worktree is an edit child's git worktree; zero for every other
	// child.
	Worktree Worktree
}

// Worktree is an edit child's own git worktree and branch (schema
// version 12).
type Worktree struct {
	Path   string // the worktree, under the state dir
	Branch string // aggy's branch for it
	Base   string // the commit it was made from
	Repo   string // the main checkout it came from
	GitDir string // its admin dir in the repo's .git
	State  string // pending, accepted, or rejected
}

// InsertChild adds a child. The parent must exist. An empty Status means
// "open".
func (s *Store) InsertChild(ctx context.Context, c Child) error {
	if c.Status == "" {
		c.Status = "open"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO children (id, parent_id, vendor, vendor_session_id, kind, title, model, cwd,
		                       transcript_path, status, created_at, last_active, vendor_home)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ParentID, c.Vendor, c.VendorSessionID, c.Kind, c.Title, c.Model, c.Cwd,
		c.TranscriptPath, c.Status, millis(c.CreatedAt), millis(c.LastActive), c.VendorHome)
	return err
}
