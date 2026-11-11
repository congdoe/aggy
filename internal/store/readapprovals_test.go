package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestReadApprovals (PR #129 review): a read pre-approval's identity is
// stored by path, replaced, kept as all 64 bits, and forgotten.
func TestReadApprovals(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const p = "/repo/certs/server.key"
	at := time.UnixMilli(1_700_000_000_123)
	for _, want := range []ReadApproval{
		{Path: p, Dev: 1, Ino: 2, Size: 3, ModNS: 4, ChangeNS: 5, At: at, Complete: true},
		{Path: p, Dev: 1<<63 + 5, Ino: 1<<64 - 1, Size: 0, ModNS: -1, ChangeNS: 0, At: at, Complete: true},
	} {
		if err := s.PutReadApproval(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.ReadApprovalFor(ctx, p)
		if err != nil || !ok || got != want {
			t.Errorf("got %+v %v %v, want %+v", got, ok, err, want)
		}
	}
	if err := s.DeleteReadApproval(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ReadApprovalFor(ctx, p); ok || err != nil {
		t.Errorf("after delete: %v %v", ok, err)
	}
}

// TestReadApprovalStampsFromV26 (T6976, CI on ubuntu): migration 27 applies
// on a database built from main's migrations 1 to 26, keeps a version 26
// row, which comes back incomplete, and takes a complete one after.
func TestReadApprovalStampsFromV26(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 27 || ms[25].version != 26 || ms[26].name != "0027_read_approval_stamps.sql" {
		t.Fatalf("migrations: %d", len(ms))
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms[:26] {
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 26`,
		`INSERT INTO read_approvals (path, dev, ino, added_at) VALUES ('/old', 1, 2, 3)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, err := s.Version(ctx); err != nil || v != len(ms) {
		t.Fatalf("version %d, %v", v, err)
	}
	if a, ok, err := s.ReadApprovalFor(ctx, "/old"); err != nil || !ok || a.Complete || a.Dev != 1 || a.Ino != 2 {
		t.Fatalf("version 26 row: %+v %v %v", a, ok, err)
	}
	want := ReadApproval{Path: "/new", Dev: 3, Ino: 4, Size: 5, ModNS: 6, ChangeNS: 7, At: time.UnixMilli(8), Complete: true}
	if err := s.PutReadApproval(ctx, want); err != nil {
		t.Fatal(err)
	}
	if a, ok, err := s.ReadApprovalFor(ctx, "/new"); err != nil || !ok || a != want {
		t.Fatalf("new row: %+v %v %v", a, ok, err)
	}
}

// TestReadApprovalLegacy (T6976, CI on ubuntu): a row written before the read_approval_stamps
// migration has device and inode only; it comes back incomplete, so it matches no
// file, and adding the rule again completes it.
func TestReadApprovalLegacy(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const p = "/repo/certs/server.key"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO read_approvals (path, dev, ino, added_at) VALUES (?, 1, 2, 3)`, p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ReadApprovalFor(ctx, p)
	if err != nil || !ok || got.Complete || got.Dev != 1 || got.Ino != 2 {
		t.Fatalf("legacy row: %+v %v %v", got, ok, err)
	}
	if err := s.PutReadApproval(ctx, ReadApproval{Path: p, Dev: 1, Ino: 2, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.ReadApprovalFor(ctx, p); !got.Complete {
		t.Errorf("added again: %+v", got)
	}
}
