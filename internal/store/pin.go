package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"io/fs"
	"os"

	"github.com/congdoe/aggy/internal/safefs"
)

// SQLite opens a database file itself, by path, outside safefs, and its
// unix VFS follows a symlink there unless sqlite3_open_v2 gets
// SQLITE_OPEN_NOFOLLOW. modernc.org/sqlite (v1.61.0) cannot pass it: the
// driver hard-codes the open flags (READWRITE|CREATE|FULLMUTEX|URI, see
// its conn.go), SQLite has no URI parameter for it, and the driver's only
// other VFS is a read-only fs.FS one. So every opener here does the best
// available form instead:
//
//   - before the open, Lstat the database: it must be a regular file with
//     one link, not a symlink, and its -wal and -shm regular or absent;
//   - after every connection SQLite opens (database/sql opens more
//     later, each reopening the path), Lstat again and refuse the
//     connection unless the file is still the one pinned, by identity,
//     and safefs still clears all three files.
//
// A swap inside the window between the two Lstats is refused, but SQLite
// may already have read or written what it opened by then (the WAL switch
// writes the header). Closing that needs the flag.

// testHookBeforeOpen, when set, runs between the pin and SQLite's open of
// a connection, so a test can swap the file in that window.
var testHookBeforeOpen func()

// pinDB returns the identity of the database file at path, refusing a
// symlink, a non-regular or multiply-linked file there or at its -wal or
// -shm. The file must exist.
func pinDB(path string) (fs.FileInfo, error) {
	fi, err := safefs.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := plainFile(path, fi); err != nil {
		return nil, err
	}
	for _, p := range []string{path + "-wal", path + "-shm"} {
		wfi, err := safefs.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if err := plainFile(p, wfi); err != nil {
			return nil, err
		}
	}
	return fi, nil
}

// plainFile refuses anything at p but a regular file with one link.
func plainFile(p string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return &fs.PathError{Op: "store", Path: p, Err: safefs.ErrNotRegular}
	}
	if safefs.LinkCount(fi) > 1 {
		return &fs.PathError{Op: "store", Path: p, Err: safefs.ErrHardLink}
	}
	return nil
}

// dbPin is a database path and the identity pinDB took for it.
type dbPin struct {
	path string
	want fs.FileInfo
}

// confirm checks, after SQLite opened the path, that it is still the
// pinned file and that safefs clears it and its WAL files.
func (p dbPin) confirm() error {
	fi, err := pinDB(p.path)
	if err != nil {
		return err
	}
	if !os.SameFile(p.want, fi) {
		return &fs.PathError{Op: "store", Path: p.path, Err: safefs.ErrChanged}
	}
	for _, f := range []string{p.path, p.path + "-wal", p.path + "-shm"} {
		if err := safefs.Check(f); err != nil {
			return err
		}
	}
	return nil
}

// pinned confirms its pins after each connection the base opens, and
// closes and refuses a connection that opened anything else.
type pinned struct {
	driver.Connector
	pins []dbPin
}

// Connect opens a connection and confirms every pin.
func (c pinned) Connect(ctx context.Context) (driver.Conn, error) {
	if testHookBeforeOpen != nil {
		testHookBeforeOpen()
	}
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range c.pins {
		if err := p.confirm(); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}
