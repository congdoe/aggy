package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Note returns the value recorded under name, and whether there is one
// (schema version 24). An index too old to have the notes table, as a
// read-only handle may open, has none.
func (s *Store) Note(ctx context.Context, name string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM notes WHERE name = ?`, name).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return v, true, nil
}

// SetNote records value under name at at, replacing any earlier value.
func (s *Store) SetNote(ctx context.Context, name, value string, at time.Time) error {
	if name == "" || value == "" {
		return fmt.Errorf("store: a note needs a name and a value")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notes (name, value, at) VALUES (?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET value = excluded.value, at = excluded.at`, name, value, millis(at))
	return err
}
