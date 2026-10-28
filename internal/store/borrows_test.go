package store

import (
	"context"
	"errors"
	"runtime"
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
	// #10-11: its child is closed too.
	var status string
	if err := s.DB().QueryRow(`SELECT status FROM children WHERE id = 'c-dead'`).Scan(&status); err != nil || status != "closed" {
		t.Errorf("child of the dead borrow is %q, %v", status, err)
	}
}

// TestBorrowUsageSince (T6698): finished borrows since a time, summed
// per vendor; running and older ones are left out.
func TestBorrowUsageSince(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	p, err := s.ParentForDir(ctx, "/repo", "borrows", "p1", now)
	if err != nil {
		t.Fatal(err)
	}
	// u4's lease stays held while it runs: a lease the collector
	// finalizes drops its lock, and the next start reaps u4 as abandoned.
	var leases []*BorrowLease
	defer func() { runtime.KeepAlive(leases) }()
	for _, b := range []struct {
		id, vendor string
		started    time.Time
		took       time.Duration // 0: still running
		tokens     int64
	}{
		{"u1", "claude", now.Add(-time.Hour), 2 * time.Minute, 100},
		{"u2", "claude", now.Add(-30 * time.Minute), time.Minute, 0},
		{"u3", "grok", now.Add(-10 * 24 * time.Hour), time.Minute, 500},
		{"u4", "codex", now.Add(-time.Minute), 0, 0},
		{"u5", "grok", now.Add(-time.Minute), 30 * time.Second, 7},
	} {
		l, err := s.StartBorrow(ctx,
			Child{ID: "c-" + b.id, ParentID: p.ID, Vendor: b.vendor, Kind: "news", Title: "t", Cwd: "/tmp/w", CreatedAt: b.started, LastActive: b.started},
			Borrow{ID: b.id, Kind: "news", Vendor: b.vendor, StartedAt: b.started}, BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
		if b.took > 0 {
			if err := l.Finish(ctx, BorrowEnd{Status: "done", Outcome: "ok", Tokens: b.tokens, EndedAt: b.started.Add(b.took)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.BorrowUsageSince(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []BorrowUsage{
		{Vendor: "claude", Borrows: 2, WithTokens: 1, Tokens: 100, Time: 3 * time.Minute},
		{Vendor: "grok", Borrows: 1, WithTokens: 1, Tokens: 7, Time: 30 * time.Second},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
