package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"

	"modernc.org/sqlite"
)

// ErrSchemaTooOld means the database predates what a read-only caller
// needs; only a normal Open, which migrates, can upgrade it.
var ErrSchemaTooOld = errors.New("store: the index is older than this command needs")

// ErrUnreadable matches an index OpenReadOnly cannot read without writing.
var ErrUnreadable = errors.New("store: cannot read the index")

// OpenReadOnly opens an existing database without creating, migrating,
// or changing any row, and every connection runs with query_only on. The
// file must exist. The caller checks Version against what it needs.
//
// How it opens depends on whether a writer could exist, which is whether
// this process may write the database's directory (safefs.DirWritable):
//
//   - Not writable: no aggy process can be writing it either, so the file
//     is opened with immutable=1, which reads the main file only and
//     creates nothing. A -wal left by a writer that crashed is not read;
//     its last transactions show once aggy runs where it can write.
//   - Writable: a writer may be live, so the file is opened with
//     mode=rw and query_only, never immutable. SQLite then reads the -wal
//     through the -shm, creating them if a WAL-mode database has none. A
//     read-only (mode=ro) connection could never remove them again; this
//     one can, and does when it is the last connection to close: SQLite
//     checkpoints the -wal into the main file (the same pages, no row
//     changes) and deletes both, as every aggy writer does on close.
//
// Callers may only run SELECT statements (and read PRAGMA user_version)
// through it. Every connection carries a SQLite authorizer that refuses
// anything else when the statement is compiled: writes, other PRAGMAs,
// ATTACH (which would open another file outside safefs), VACUUM, and so
// on.
//
// The paths are checked through safefs before the open and, as Open does,
// again after it, by identity. A read that meets a writer's checkpoint
// can fail with a "malformed" error; it is reported as the index being
// written, to be retried, never as damage (see ReadError).
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
	writable, err := safefs.DirWritable(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	base, err := sqlite.NewConnector(roDSN(path, !writable))
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(selectOnly{base})
	// Ping opens a connection; a query proves the file can be read.
	err = db.PingContext(ctx)
	if err == nil {
		_, err = (&Store{db: db}).Version(ctx)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w %s: %w", ErrUnreadable, path, ReadError(err))
	}
	// SQLite opened the files itself, outside safefs. Confirm by identity
	// that none of them is a vendor credential file.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := safefs.Check(p); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Store{db: db, path: path}, nil
}

// selectOnly opens connections that may only read: an authorizer is
// installed on each before database/sql uses it. The DSN's own pragmas
// run before it, while the connection opens.
type selectOnly struct{ driver.Connector }

// Connect opens a connection and installs the read-only authorizer.
func (c selectOnly) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	reg, ok := conn.(sqlite.AuthorizerRegisterer)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("store: the SQLite driver has no authorizer")
	}
	if err := reg.RegisterAuthorizer(readOnlyAuthorizer); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// readOnlyAuthorizer allows reading tables, functions, and recursive
// queries, and the one PRAGMA read aggy uses, user_version, without a
// value. Everything else is denied when the statement is compiled.
func readOnlyAuthorizer(action sqlite.AuthorizerActionCode, arg1, arg2, _, _ string) sqlite.AuthorizerReturnCode {
	switch action {
	case sqlite.AuthSelect, sqlite.AuthRead, sqlite.AuthFunction, sqlite.AuthRecursive:
		return sqlite.AuthorizerOK
	case sqlite.AuthPragma:
		if strings.EqualFold(arg1, "user_version") && arg2 == "" {
			return sqlite.AuthorizerOK
		}
	}
	return sqlite.AuthorizerDeny
}

// ErrBusyWriting matches a read that met a writer mid-checkpoint.
var ErrBusyWriting = errors.New("store: the index is being written; try again")

// ReadError explains err from a read-only query: SQLite's "malformed" and
// "corrupt" errors during a read mean a writer changed pages under the
// read, so they match ErrBusyWriting, which says to retry. Never delete
// the index over one. Other errors come back unchanged.
func ReadError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "malformed") || strings.Contains(msg, "corrupt") {
		return fmt.Errorf("%w (%w)", ErrBusyWriting, err)
	}
	return err
}

// roDSN names exactly path, never creating it: immutable=1 (read-only)
// when asked, else mode=rw. query_only is on either way.
func roDSN(path string, immutable bool) string {
	q := url.Values{}
	if immutable {
		q.Add("mode", "ro")
		q.Add("immutable", "1")
	} else {
		q.Add("mode", "rw")
	}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}
