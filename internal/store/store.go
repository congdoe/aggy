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

// lockWait bounds the wait for another process's first-time setup.
const lockWait = 30 * time.Second

// ErrLockTimeout means another process held the setup lock for longer than
// the store waits.
var ErrLockTimeout = errors.New("store: timed out waiting for the setup lock")

// ErrLockUnsupported means the file system holding the store cannot lock
// files. Open goes on without the lock and reports it in Warnings: the
// migrations still run in BEGIN IMMEDIATE transactions, so racing setups
// cannot apply one twice, but a race on the first WAL switch can fail with
// SQLITE_BUSY instead of waiting.
var ErrLockUnsupported = errors.New("store: file locks are not supported here")

// Store is the open index database.
type Store struct {
	db       *sql.DB
	path     string
	warnings []error
}

// Warnings returns problems Open worked around, such as
// ErrLockUnsupported.
func (s *Store) Warnings() []error { return s.warnings }

// Path returns the absolute path of the database file.
func (s *Store) Path() string { return s.path }

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

// Open opens or creates the database at path and migrates it. A directory
// it creates is 0700; an existing directory keeps its mode. The file is
// 0600. Every connection runs with busy_timeout, WAL, and foreign_keys on.
func Open(ctx context.Context, path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if _, err := safefs.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := safefs.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	// Create the file here, through safefs, so it starts 0600 and is
	// checked; SQLite gives the -wal and -shm files the same mode.
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

	// Serialize first-time setup (the WAL switch and migrations) across
	// processes and goroutines; racing it fails with SQLITE_BUSY. The wait
	// is bounded and honors ctx.
	var warnings []error
	unlock, err := lockFile(ctx, path+".lock", lockWait)
	if errors.Is(err, ErrLockUnsupported) {
		warnings = append(warnings, err)
	} else if err != nil {
		return nil, err
	}
	defer unlock()

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path, warnings: warnings}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite opened the file itself, outside safefs. Confirm by identity
	// that neither it nor its WAL files is a vendor credential file.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := safefs.Check(p); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds a file: URI that names exactly path. SQLite percent-decodes
// the URI path and splits it at '?' and '#', so the path is escaped rather
// than concatenated: "auth.jso%6E" must open a file of that name, not
// auth.json.
func dsn(path string) string {
	q := url.Values{}
	// busy_timeout first, so the pragmas after it wait instead of failing.
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
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
	entries, err := migrationFS.ReadDir("migrations")
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

// LockFile takes an exclusive lock on path, creating it 0600, and returns
// the unlock function. It waits at most wait and honors ctx, returning
// ErrLockTimeout or ctx's error. On a file system without file locks it
// returns a no-op unlock and an error matching ErrLockUnsupported, so the
// caller can choose to go on unlocked.
func LockFile(ctx context.Context, path string, wait time.Duration) (func(), error) {
	return lockFile(ctx, path, wait)
}
