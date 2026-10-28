package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionsRows(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_000_000).UTC()
	for _, p := range []Parent{
		{ID: "aaaa1111", Dir: "/a", CreatedAt: t0, LastActive: t0},
		{ID: "aaaa2222", Dir: "/a", CreatedAt: t0, LastActive: t0.Add(time.Minute)},
		{ID: "bbbb1111", Dir: "/b", CreatedAt: t0, LastActive: t0.Add(2 * time.Minute)},
	} {
		if err := s.InsertParent(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.LatestParentInDir(ctx, "/a")
	if err != nil || p.ID != "aaaa2222" {
		t.Errorf("LatestParentInDir(/a) = %v, %v", p.ID, err)
	}
	if _, err := s.LatestParentInDir(ctx, "/c"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestParentInDir(/c) err = %v", err)
	}
	ps, err := s.Parents(ctx)
	if err != nil || len(ps) != 3 || ps[0].ID != "bbbb1111" {
		t.Errorf("Parents = %v, %v", ps, err)
	}
	tests := []struct {
		prefix string
		n      int
		bad    bool
	}{
		{"aaaa", 2, false}, {"aaaa1", 1, false}, {"c", 0, false},
		{"", 0, true}, {"a%", 0, true}, {"A", 0, true},
	}
	for _, tt := range tests {
		got, err := s.ParentsByPrefix(ctx, tt.prefix)
		if (err != nil) != tt.bad || len(got) != tt.n {
			t.Errorf("ParentsByPrefix(%q) = %d rows, %v", tt.prefix, len(got), err)
		}
	}

	c := Child{ID: "c1", ParentID: "aaaa1111", Vendor: "codex", Cwd: "/a", CreatedAt: t0, LastActive: t0}
	if err := s.InsertChild(ctx, c); err != nil {
		t.Fatal(err)
	}
	c2 := Child{ID: "c2", ParentID: "aaaa1111", Vendor: "claude", VendorSessionID: "s2", Cwd: "/a", CreatedAt: t0, LastActive: t0}
	if err := s.InsertChild(ctx, c2); err != nil {
		t.Fatal(err)
	}

	// OnScreen raises the child and parent and sets the last vendor.
	t1 := t0.Add(10 * time.Minute)
	if err := s.OnScreen(ctx, "aaaa1111", "c1", "codex", t1); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Children(ctx, "aaaa1111")
	if len(cs) != 2 || cs[0].ID != "c1" || !cs[0].LastActive.Equal(t1) {
		t.Errorf("Children after OnScreen = %+v", cs)
	}
	p, _ = s.GetParent(ctx, "aaaa1111")
	if p.LastVendor != "codex" || !p.LastActive.Equal(t1) {
		t.Errorf("parent after OnScreen = %+v", p)
	}

	// SetChildActivity never lowers last_active, and sets status.
	if err := s.SetChildActivity(ctx, "c2", t0.Add(-time.Hour), "gone"); err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(time.Minute)
	if err := s.SetChildActivity(ctx, "c1", t2, ""); err != nil {
		t.Fatal(err)
	}
	cs, _ = s.Children(ctx, "aaaa1111")
	if cs[0].ID != "c1" || !cs[0].LastActive.Equal(t2) || cs[1].Status != "gone" || !cs[1].LastActive.Equal(t0) {
		t.Errorf("Children after SetChildActivity = %+v", cs)
	}
	if p, _ = s.GetParent(ctx, "aaaa1111"); !p.LastActive.Equal(t2) {
		t.Errorf("parent last active = %v, want %v", p.LastActive, t2)
	}

	// SetChildSessionID: once, idempotent, never replaced, unique.
	if err := s.SetChildSessionID(ctx, "c1", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildSessionID(ctx, "c1", "t1"); err != nil {
		t.Errorf("same id again: %v", err)
	}
	if err := s.SetChildSessionID(ctx, "c1", "t9"); !errors.Is(err, ErrSessionSet) {
		t.Errorf("another id: %v, want ErrSessionSet", err)
	}
	if err := s.SetChildSessionID(ctx, "nope", "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing child: %v, want ErrNotFound", err)
	}
	c3 := Child{ID: "c3", ParentID: "aaaa1111", Vendor: "codex", Cwd: "/a", CreatedAt: t0, LastActive: t0}
	if err := s.InsertChild(ctx, c3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildSessionID(ctx, "c3", "t1"); err == nil {
		t.Error("a second codex child took the same thread id")
	}
}
