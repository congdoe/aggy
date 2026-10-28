package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"

	"github.com/congdoe/aggy/internal/safefs"
)

// ErrSchemaTooOld means the database predates what a read-only caller
// needs; only a normal Open, which migrates, can upgrade it.
var ErrSchemaTooOld = errors.New("store: the index is older than this command needs")

// ErrUnreadable matches an index OpenReadOnly cannot read without writing.
var ErrUnreadable = errors.New("store: cannot read the index")

// OpenReadOnly opens an existing database without creating, locking,
// migrating, or writing anything, and every connection runs with
// query_only on. The file must exist. The caller checks Version against
// what it needs.
//
// SQLite's read-only mode still creates the -wal and -shm files of a WAL
// database, and cannot remove them on close, so a plain mode=ro open would
// leave both behind, and fails outright in a directory it cannot write.
// OpenReadOnly therefore picks the open by what is on disk:
//
//   - No -wal and no -shm: no connection has the database open in WAL
//     mode, and the last one checkpointed everything into the main file
//     when it closed. The file is opened with immutable=1, which reads the
//     main file only and creates nothing, so it works in a read-only
//     directory and leaves nothing behind. If a writer starts during the
//     read, its changes stay in its own -wal until a checkpoint; a
//     checkpoint that runs during a read can make that read fail with a
//     corruption error, never write anything. aggy's read-only callers
//     (aggy log, the setup mode check) are short and can be run again.
//   - A -wal or -shm exists: a writer is live, or one exited without
//     cleaning up, and the -wal may hold committed data the main file does
//     not, so immutable=1 would read stale data. The file is opened with
//     mode=ro, which reads the -wal. The files were already there. If
//     SQLite cannot open them read-only (a -wal but no -shm in a directory
//     it cannot write), the error matches ErrUnreadable and says so.
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
	immutable := true
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if _, err := safefs.Lstat(p); err == nil {
			immutable = false
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", roDSN(path, immutable))
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
		if immutable {
			return nil, fmt.Errorf("%w %s: %w", ErrUnreadable, path, err)
		}
		return nil, fmt.Errorf("%w %s: %w (it has a WAL file that SQLite cannot open read-only; run an aggy command that writes, with write access to %s, to recover it)",
			ErrUnreadable, path, err, filepath.Dir(path))
	}
	return &Store{db: db, path: path}, nil
}

// roDSN names exactly path, read-only, with query_only on, and with
// immutable=1 when asked.
func roDSN(path string, immutable bool) string {
	q := url.Values{}
	q.Add("mode", "ro")
	if immutable {
		q.Add("immutable", "1")
	}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}
