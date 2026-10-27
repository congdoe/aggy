package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/congdoe/aggy/internal/safefs"
)

// ErrSchemaTooOld means the database predates what a read-only caller
// needs; only a normal Open, which migrates, can upgrade it.
var ErrSchemaTooOld = errors.New("store: the index is older than this command needs")

// OpenReadOnly opens an existing database without creating, locking,
// migrating, or writing anything: SQLite opens it with mode=ro, and every
// connection runs with query_only on. The file must exist. The caller
// checks Version against what it needs.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// The file and its WAL files must not be vendor credential files, and
	// the file must exist; safefs checks both without creating anything.
	if _, err := safefs.Stat(path); err != nil {
		return nil, err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := safefs.Check(p); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", roDSN(path))
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s read-only: %w", path, err)
	}
	return &Store{db: db, path: path}, nil
}

// roDSN names exactly path, read-only, with query_only on.
func roDSN(path string) string {
	q := url.Values{}
	q.Add("mode", "ro")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}
