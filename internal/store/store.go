package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// FileName is the database file inside the state directory.
const FileName = "aggy.db"

// busyTimeout is how long a connection waits for another writer.
const busyTimeout = 5 * time.Second

// Store is the open index database.
type Store struct {
	db *sql.DB
}

// DefaultPath returns the database path in the state directory
// ($AGGY_HOME or ~/.aggy).
func DefaultPath() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// OpenDefault opens the database at DefaultPath.
func OpenDefault(ctx context.Context) (*Store, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Open(ctx, path)
}

// Open opens or creates the database at path and migrates it. The directory
// is created 0700 and the file 0600. Every connection runs with WAL,
// busy_timeout, and foreign_keys on.
func Open(ctx context.Context, path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := safefs.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := safefs.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	// Create the file here so it starts 0600; SQLite gives the -wal and
	// -shm files the same mode.
	f, err := safefs.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := safefs.Chmod(path, 0o600); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + q.Encode()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying handle for queries the helpers do not cover.
func (s *Store) DB() *sql.DB { return s.db }

type migration struct {
	version int
	name    string
	sql     string
}

// migrations returns the embedded files in version order. Each file is
// named NNNN_description.sql and NNNN is its schema version.
func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("store: bad migration name %q", e.Name())
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration %s out of sequence (want version %d)", m.name, i+1)
		}
	}
	return ms, nil
}

// Migrate applies every migration newer than PRAGMA user_version. Each one
// runs in its own write transaction (BEGIN IMMEDIATE) that also bumps the
// version, so a crash leaves the schema at a whole version and two processes
// cannot apply the same migration twice. Running it again is a no-op.
func (s *Store) Migrate(ctx context.Context) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	for _, m := range ms {
		if err := apply(ctx, conn, m); err != nil {
			return fmt.Errorf("store: migration %s: %w", m.name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *sql.Conn, m migration) (err error) {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	var cur int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return err
	}
	if cur >= m.version {
		_, err = conn.ExecContext(ctx, "COMMIT")
		return err
	}
	if _, err := conn.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// Version returns the schema version (PRAGMA user_version).
func (s *Store) Version(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// ErrNotFound is returned by the Get helpers when no row matches.
var ErrNotFound = errors.New("store: not found")

func millis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
