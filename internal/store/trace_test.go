//go:build unix

package store

import (
	"context"
	"testing"
	"time"
)

// TestTraceColumns (T6753, P58-4): a borrow's and a child's trace are
// written with the row (version 15) and read back by the borrow lookups
// and gc; a row with none reads "". A read-only open of a version 14
// index, which has no trace column, still reads, with no traces.
func TestTraceColumns(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertParent(ctx, Parent{ID: "p", Dir: "/x", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct{ id, trace string }{{"aaaaaaaaaaaaaaaa", "T1"}, {"bbbbbbbbbbbbbbbb", ""}} {
		c := Child{ID: "c" + x.id, ParentID: "p", Vendor: "codex", Kind: "edit", Cwd: "/x", CreatedAt: now, LastActive: now,
			TraceID: x.trace, Worktree: Worktree{Path: "/s/worktrees/" + x.id, Branch: "aggy/edit-" + x.id, Repo: "/x", State: WorktreePending}}
		l, err := s.StartBorrow(ctx, c, Borrow{ID: x.id, Kind: "edit", Vendor: "codex", StartedAt: now, TraceID: x.trace}, BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Finish(ctx, BorrowEnd{Status: "done", EndedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-48 * time.Hour)
	if err := s.InsertChild(ctx, Child{ID: "cimg", ParentID: "p", Vendor: "grok", Kind: "image", Cwd: "/x", Status: "closed",
		CreatedAt: old, LastActive: old, TraceID: "T2"}); err != nil {
		t.Fatal(err)
	}

	bs, err := s.BorrowsByPrefix(ctx, "")
	if err != nil || len(bs) != 2 || bs[0].TraceID != "T1" || bs[1].TraceID != "" {
		t.Fatalf("BorrowsByPrefix = %+v, %v", bs, err)
	}
	if b, err := s.GetBorrow(ctx, "aaaaaaaaaaaaaaaa"); err != nil || b.TraceID != "T1" {
		t.Errorf("GetBorrow = %+v, %v", b, err)
	}
	var childTrace string
	if err := s.db.QueryRowContext(ctx, `SELECT trace_id FROM children WHERE id = 'caaaaaaaaaaaaaaaa'`).Scan(&childTrace); err != nil || childTrace != "T1" {
		t.Errorf("child trace = %q, %v", childTrace, err)
	}
	var null int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM borrows WHERE trace_id IS NULL`).Scan(&null); err != nil || null != 1 {
		t.Errorf("borrows with no trace = %d, %v (want NULL, not '')", null, err)
	}
	check := func(s *Store, want1, want2 string) {
		t.Helper()
		ws, err := s.GCWorktrees(ctx)
		if err != nil || len(ws) != 2 || ws[0].TraceID != want1 || ws[1].TraceID != "" {
			t.Errorf("GCWorktrees = %+v, %v", ws, err)
		}
		cs, err := s.GCChildren(ctx, now)
		if err != nil || len(cs) != 1 || cs[0].ID != "cimg" || cs[0].TraceID != want2 {
			t.Errorf("GCChildren = %+v, %v", cs, err)
		}
	}
	check(s, "T1", "T2")

	// Back to a real version 14 file: no trace columns.
	for _, q := range []string{
		`ALTER TABLE borrows DROP COLUMN trace_id`,
		`ALTER TABLE children DROP COLUMN trace_id`,
		`PRAGMA user_version = 14`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	check(ro, "", "")
}
