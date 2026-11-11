package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Key is one key aggy key stored: its name and the OpenCode provider it is
// bound to (schema version 29). The value is never in the index: it is in the
// OS keychain under that name.
type Key struct {
	Name     string
	Provider string
	AddedAt  time.Time
}

// Keys returns the stored keys by name. An index too old to have the table,
// as a read-only handle may open, has none.
func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, provider, added_at FROM keys ORDER BY name`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Key
	for rows.Next() {
		var k Key
		var at int64
		if err := rows.Scan(&k.Name, &k.Provider, &at); err != nil {
			return nil, err
		}
		k.AddedAt = fromMillis(at)
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeyNamed returns the key called name, and whether there is one.
func (s *Store) KeyNamed(ctx context.Context, name string) (Key, bool, error) {
	var k Key
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT name, provider, added_at FROM keys WHERE name = ?`, name).Scan(&k.Name, &k.Provider, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Key{}, false, nil
	case err != nil && strings.Contains(err.Error(), "no such table"):
		return Key{}, false, nil
	case err != nil:
		return Key{}, false, err
	}
	k.AddedAt = fromMillis(at)
	return k, true, nil
}

// PutKey records k, replacing a key of the same name. A provider another name
// is bound to is an error (the table's UNIQUE constraint).
func (s *Store) PutKey(ctx context.Context, k Key) error {
	if k.Name == "" || k.Provider == "" {
		return fmt.Errorf("store: a key needs a name and a provider")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO keys (name, provider, added_at) VALUES (?, ?, ?)
		 ON CONFLICT (name) DO UPDATE SET provider = excluded.provider, added_at = excluded.added_at`,
		k.Name, k.Provider, millis(k.AddedAt))
	return err
}

// DeleteKey removes the key called name; none is not an error.
func (s *Store) DeleteKey(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM keys WHERE name = ?`, name)
	return err
}
