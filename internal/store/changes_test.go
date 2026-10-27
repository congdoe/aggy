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
	c.ID = got.ID
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
