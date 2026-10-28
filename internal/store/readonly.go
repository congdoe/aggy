package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
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
	db, err := sql.Open("sqlite", roDSN(path, !writable))
	if err != nil {
		return nil, err
	}
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
