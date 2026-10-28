package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenReadOnly(t *testing.T) {
	s, path := openTemp(t)
	_ = s.Close()
	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if v, err := ro.Version(ctx); err != nil || v < 1 {
		t.Errorf("Version = %d, %v", v, err)
	}
	if _, err := ro.DB().ExecContext(ctx, `INSERT INTO events (at, type) VALUES (1, 'x')`); err == nil {
		t.Error("a read-only store accepted a write")
	}
	missing := filepath.Join(t.TempDir(), FileName)
	if _, err := OpenReadOnly(ctx, missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenReadOnly(missing) = %v, want ErrNotExist", err)
	}
}

// TestOpenReadOnlyLeavesNothing (T6725 N5): reading a cleanly closed index
// creates no -wal or -shm file. On 2e1e234 both were left behind.
func TestOpenReadOnlyLeavesNothing(t *testing.T) {
	s, path := openTemp(t)
	_ = s.Close()
	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := ro.Version(ctx); err != nil || v < 1 {
		t.Errorf("Version = %d, %v", v, err)
	}
	var n int
	if err := ro.DB().QueryRowContext(ctx, `SELECT count(*) FROM changes`).Scan(&n); err != nil {
		t.Error(err)
	}
	_ = ro.Close()
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s left behind: %v", filepath.Base(p), err)
		}
	}
}

// TestOpenReadOnlyInReadOnlyDir (T6725 N4): a cleanly closed index in a
// directory aggy cannot write is still readable. On 2e1e234 the open
// failed with "attempt to write a readonly database".
func TestOpenReadOnlyInReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, path := openTemp(t)
	_ = s.Close()
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if v, err := ro.Version(ctx); err != nil || v < 1 {
		t.Errorf("Version = %d, %v", v, err)
	}
}

// TestOpenReadOnlySeesLiveWAL: with a writer open, the read sees its
// committed rows (from the -wal), and the writer's files stay.
func TestOpenReadOnlySeesLiveWAL(t *testing.T) {
	s, path := openTemp(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO events (at, type) VALUES (1, 'live')`); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	var n int
	if err := ro.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE type = 'live'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("live rows = %d, %v, want 1", n, err)
	}
}

// TestOpenReadOnlySeesWriterThatStartsLater (PR #6 finding 13): in a
// writable directory a writer can start after the read-only open, so
// the read must see its committed rows. On a8b4c37 the open was
// immutable=1 whenever no -wal existed, and read stale data.
func TestOpenReadOnlySeesWriterThatStartsLater(t *testing.T) {
	s, path := openTemp(t)
	_ = s.Close()
	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	w, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.DB().ExecContext(ctx, `INSERT INTO events (at, type) VALUES (1, 'later')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ro.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE type = 'later'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows from the later writer = %d, %v, want 1", n, err)
	}
}

// TestReadErrorNeverSaysDelete: a "malformed" read is reported as the
// index being written, to retry.
func TestReadErrorNeverSaysDelete(t *testing.T) {
	err := ReadError(errors.New("database disk image is malformed (11)"))
	if !errors.Is(err, ErrBusyWriting) || strings.Contains(strings.ToLower(err.Error()), "delete") {
		t.Errorf("ReadError = %v", err)
	}
	other := errors.New("no such table: x")
	if ReadError(other) != other {
		t.Error("ReadError changed an unrelated error")
	}
}
