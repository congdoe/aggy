package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestChangeLifecycle(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	at := time.UnixMilli(time.Now().UnixMilli()).UTC()
	c := Change{ChangeID: "c1", RunID: "run1", File: "/x/config.toml", HashBefore: "aa", HashAfter: "bb", PlannedAt: at}
	if err := s.InsertChange(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChange(ctx, "c1")
	if err != nil || got.Status != ChangePending || !got.AppliedAt.IsZero() || got.CreatedDirs == nil {
		t.Fatalf("pending = %+v, %v", got, err)
	}
	c.Status, c.BackupPath, c.FileMode, c.AppliedAt = ChangeApplied, "/b/c1/aa.bak", 0o640, at
	c.CreatedFile, c.CreatedDirs = true, []string{"/x"}
	if err := s.FinishChange(ctx, c); err != nil {
		t.Fatal(err)
	}
	// Only a pending change can be finished.
	if err := s.FinishChange(ctx, c); !errors.Is(err, ErrNotFound) {
		t.Errorf("second FinishChange = %v, want ErrNotFound", err)
	}
	got, err = s.GetChange(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	c.ID, c.Kind = got.ID, KindChange
	if !reflect.DeepEqual(got, c) {
		t.Errorf("applied = %+v\nwant %+v", got, c)
	}
	if err := s.MarkChangeUndone(ctx, "c1", at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkChangeUndone(ctx, "c1", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("second undo = %v, want ErrNotFound", err)
	}
	if err := s.InsertChange(ctx, Change{ChangeID: "c2", RunID: "run2", File: "/y", HashBefore: "none", HashAfter: "cc", PlannedAt: at}); err != nil {
		t.Fatal(err)
	}
	if cs, err := s.Changes(ctx, "run1"); err != nil || len(cs) != 1 || cs[0].Status != ChangeUndone || !cs[0].UndoneAt.Equal(at) {
		t.Errorf("Changes(run1) = %+v, %v", cs, err)
	}
	if cs, err := s.Changes(ctx, ""); err != nil || len(cs) != 2 {
		t.Errorf("Changes(all) = %d, %v", len(cs), err)
	}
	// change_id is unique, and status is checked.
	if err := s.InsertChange(ctx, Change{ChangeID: "c2", File: "/z", PlannedAt: at}); err == nil {
		t.Error("duplicate change_id accepted")
	}
	if err := s.InsertChange(ctx, Change{ChangeID: "c3", File: "/z", Status: "bogus", PlannedAt: at}); err == nil {
		t.Error("bad status accepted")
	}
}

// TestMigrateChangesFromV1: rows written under schema 1 survive the
// rebuild in 0002 as applied or undone changes.
func TestMigrateChangesFromV1(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		ms[0].sql,
		`PRAGMA user_version = 1`,
		`INSERT INTO changes (file, hash_before, hash_after, backup_path, applied_at) VALUES ('/a', 'none', 'h1', '', 1000)`,
		`INSERT INTO changes (file, hash_before, hash_after, backup_path, applied_at, undone_at) VALUES ('/b', 'h2', 'h3', '/bk', 2000, 3000)`,
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
	cs, err := s.Changes(ctx, "")
	if err != nil || len(cs) != 2 {
		t.Fatalf("Changes = %+v, %v", cs, err)
	}
	if c := cs[0]; c.ChangeID != "v1-1" || !c.CreatedFile || c.Status != ChangeApplied || c.AppliedAt.UnixMilli() != 1000 {
		t.Errorf("row 1 = %+v", c)
	}
	if c := cs[1]; c.CreatedFile || c.Status != ChangeUndone || c.BackupPath != "/bk" || c.UndoneAt.UnixMilli() != 3000 {
		t.Errorf("row 2 = %+v", c)
	}
}

// TestMarkerRows (T6725 N7): a marker row has its own kind, rows from
// before version 5 are classified, and ChangesNewestFirst leaves markers
// out.
func TestMarkerRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms[:4] {
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 4`,
		`INSERT INTO changes (change_id, run_id, file, hash_before, hash_after, status, planned_at) VALUES ('old-marker', 'r', '', '', '', 'noop', 1)`,
		`INSERT INTO changes (change_id, run_id, file, hash_before, hash_after, status, planned_at) VALUES ('old-change', 'r', '/a', 'none', 'h', 'applied', 2)`,
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
	at := time.Unix(10, 0)
	if err := s.InsertChange(ctx, Change{ChangeID: "new-marker", RunID: "r", Status: ChangeNoop, Kind: KindMarker, PlannedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChange(ctx, Change{ChangeID: "bad", RunID: "r", Kind: "bogus", PlannedAt: at}); err == nil {
		t.Error("bad kind accepted")
	}
	for id, want := range map[string]string{"old-marker": KindMarker, "old-change": KindChange, "new-marker": KindMarker} {
		if c, err := s.GetChange(ctx, id); err != nil || c.Kind != want {
			t.Errorf("%s kind = %q, %v, want %q", id, c.Kind, err, want)
		}
	}
	for _, run := range []string{"r", ""} {
		cs, err := s.ChangesNewestFirst(ctx, run)
		if err != nil || len(cs) != 1 || cs[0].ChangeID != "old-change" {
			t.Errorf("ChangesNewestFirst(%q) = %+v, %v, want only old-change", run, cs, err)
		}
	}
}

// TestUnlockedBackfill (B14-6): a row written before schema 8 did not
// record whether its plan held the lock, so it comes back Unlocked; rows
// written since record the value they are given.
func TestUnlockedBackfill(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms[:7] {
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 7`,
		`INSERT INTO changes (change_id, run_id, file, hash_before, hash_after, status, planned_at) VALUES ('old', 'r', '/a', 'none', 'h', 'applied', 1)`,
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
	at := time.Unix(10, 0)
	for _, c := range []Change{
		{ChangeID: "locked", RunID: "r", File: "/b", HashBefore: "none", HashAfter: "h", PlannedAt: at},
		{ChangeID: "unlocked", RunID: "r", File: "/c", HashBefore: "none", HashAfter: "h", PlannedAt: at, Unlocked: true},
	} {
		if err := s.InsertChange(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]bool{"old": true, "locked": false, "unlocked": true} {
		if c, err := s.GetChange(ctx, id); err != nil || c.Unlocked != want {
			t.Errorf("%s Unlocked = %v, %v, want %v", id, c.Unlocked, err, want)
		}
	}
}

// TestClaudeTrustReverseFromV25 (T6977): migration 26 rebuilds changes on a
// database at main's version 25 (read_approvals there), keeps every row and
// index, and then takes a claude-trust reverse while still refusing others.
func TestClaudeTrustReverseFromV25(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 26 || ms[24].version != 25 || ms[25].version != 26 {
		t.Fatalf("migrations: %d", len(ms))
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms[:25] {
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`PRAGMA user_version = 25`,
		`INSERT INTO changes (change_id, run_id, file, hash_before, hash_after, status, planned_at, reverse) VALUES ('old', 'r', '/a', 'none', 'h', 'applied', 1, 'aggy-hooks')`,
		`INSERT INTO read_approvals (path, dev, ino, added_at) VALUES ('/x', 1, 2, 3)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO changes (change_id, file, hash_before, hash_after, planned_at, reverse) VALUES ('ct', '/c', 'h', 'h', 1, 'claude-trust:nokey:/w')`); err == nil {
		t.Fatal("version 25 took a claude-trust reverse")
	}
	_ = db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, err := s.Version(ctx); err != nil || v != 26 {
		t.Fatalf("version %d, %v", v, err)
	}
	if c, err := s.GetChange(ctx, "old"); err != nil || c.Reverse != "aggy-hooks" || c.File != "/a" {
		t.Fatalf("old row %+v, %v", c, err)
	}
	at := time.Unix(10, 0)
	if err := s.InsertChange(ctx, Change{ChangeID: "ct", RunID: "r", File: "/c", HashBefore: "h", HashAfter: "h2", PlannedAt: at, Reverse: "claude-trust:nokey:/w"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChange(ctx, Change{ChangeID: "bad", RunID: "r", File: "/d", HashBefore: "h", HashAfter: "h2", PlannedAt: at, Reverse: "rm -rf"}); err == nil {
		t.Fatal("an unknown reverse was taken")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name IN ('changes_file', 'changes_run')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("indexes %d, %v", n, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM read_approvals`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read_approvals %d, %v", n, err)
	}
}
