//go:build unix

package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestStrayLeases (T6694): a held lease is never stray; a lease whose
// lock is gone while its pid lives is listed (that pid cannot be shown
// to be the holder), and gc kills nothing either way.
func TestStrayLeases(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	seedSession(t, s, "c1", "grok", "s1")
	seedSession(t, s, "c2", "grok", "s2")
	l1, err := s.AcquireLease(ctx, "grok", "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l1.Release(ctx) }()
	l2, err := s.AcquireLease(ctx, "grok", "s2")
	if err != nil {
		t.Fatal(err)
	}
	// c2's holder looks gone: its lock file is missing, its pid (ours) alive.
	l2.unlock()
	if err := os.Remove(l2.lock); err != nil {
		t.Fatal(err)
	}
	got, err := s.StrayLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c2" || got[0].PID != os.Getpid() {
		t.Errorf("stray = %+v", got)
	}
}

// TestGCWorktreesBeforeV14 (T6695): a dry run reads an index it cannot
// migrate; there a rejected worktree counts as removed, as the
// migration will mark it.
func TestGCWorktreesBeforeV14(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertParent(ctx, Parent{ID: "p", Dir: "/x", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
		c := Child{ID: "c" + id, ParentID: "p", Vendor: "codex", Kind: "edit", Cwd: "/x", CreatedAt: now, LastActive: now,
			Worktree: Worktree{Path: "/s/worktrees/" + id, Branch: "aggy/edit-" + id, Repo: "/x", State: WorktreePending}}
		l, err := s.StartBorrow(ctx, c, Borrow{ID: id, Kind: "edit", Vendor: "codex", StartedAt: now}, BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Finish(ctx, BorrowEnd{Status: "done", EndedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetWorktreeState(ctx, "aaaaaaaaaaaaaaaa", WorktreePending, WorktreeRejected); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{gcSchema, gcSchema - 1} {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			t.Fatal(err)
		}
		got, err := s.GCWorktrees(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := 2 // version 14: the reject never claimed its removal here
		if v < gcSchema {
			want = 1
		}
		if len(got) != want || got[len(got)-1].BorrowID != "bbbbbbbbbbbbbbbb" {
			t.Errorf("version %d: %+v", v, got)
		}
	}
}
