package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func startBorrow(t *testing.T, s *Store, id, vendor string, caps BorrowCaps) (*BorrowLease, error) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	p, err := s.ParentForDir(ctx, "/repo", "borrows", "p1", now)
	if err != nil {
		t.Fatal(err)
	}
	return s.StartBorrow(ctx,
		Child{ID: "c-" + id, ParentID: p.ID, Vendor: vendor, Kind: "news", Title: "t", Cwd: "/tmp/w", CreatedAt: now, LastActive: now},
		Borrow{ID: id, Kind: "news", Vendor: vendor, Budget: BorrowBudget{Minutes: 5, Turns: 15}, StartedAt: now}, caps)
}

// TestBorrowCaps (T6622): at most 2 running per vendor and 4 in all; a
// finished borrow stops counting.
func TestBorrowCaps(t *testing.T) {
	s, _ := openTemp(t)
	caps := BorrowCaps{PerVendor: 2, Total: 4}
	steps := []struct {
		id, vendor string
		wantErr    string
	}{
		{"b1", "grok", ""},
		{"b2", "grok", ""},
		{"b3", "grok", "2 grok borrows are already running (at most 2 per vendor)"},
		{"b4", "codex", ""},
		{"b5", "codex", ""},
		{"b6", "claude", "4 borrows are already running (at most 4 in all)"},
	}
	leases := map[string]*BorrowLease{}
	for _, st := range steps {
		l, err := startBorrow(t, s, st.id, st.vendor, caps)
		if st.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: %v", st.id, err)
			}
			leases[st.id] = l
			continue
		}
		if !errors.Is(err, ErrCap) || err.Error() != st.wantErr+"; wait for one to finish" {
			t.Fatalf("%s: err = %v", st.id, err)
		}
		if _, gerr := s.GetBorrow(context.Background(), st.id); !errors.Is(gerr, ErrNotFound) {
			t.Errorf("%s: a refused borrow was recorded", st.id)
		}
	}
	if err := leases["b1"].Finish(context.Background(), BorrowEnd{Status: "done", Outcome: "ok", EndedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := startBorrow(t, s, "b7", "grok", caps); err != nil {
		t.Errorf("after one finished: %v", err)
	}
	b, err := s.GetBorrow(context.Background(), "b1")
	if err != nil || b.Status != "done" || b.EndedAt.IsZero() || b.Budget.Turns != 15 {
		t.Errorf("finished row %+v, %v", b, err)
	}
}

// TestBorrowCapsReclaimDead (T6622): a running borrow whose holder died
// (its lock is free) stops counting and is marked killed.
func TestBorrowCapsReclaimDead(t *testing.T) {
	s, _ := openTemp(t)
	caps := BorrowCaps{PerVendor: 1, Total: 4}
	l, err := startBorrow(t, s, "dead", "grok", caps)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the holder dying: drop its lock without finishing.
	l.unlock()
	if _, err := startBorrow(t, s, "next", "grok", caps); err != nil {
		t.Fatalf("a dead holder still counts: %v", err)
	}
	b, _ := s.GetBorrow(context.Background(), "dead")
	if b.Status != "killed" || b.Outcome != "abandoned" {
		t.Errorf("dead borrow = %+v", b)
	}
}
