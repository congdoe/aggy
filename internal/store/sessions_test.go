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
		{ID: "aaaa2222", Dir: "/A/", DirKey: "/a", CreatedAt: t0, LastActive: t0.Add(time.Minute)},
		{ID: "bbbb1111", Dir: "/b", DirKey: "/b", CreatedAt: t0, LastActive: t0.Add(2 * time.Minute)},
	} {
		if err := s.InsertParent(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// By canonical key, and by dir for a row without one.
	p, err := s.LatestParentInDir(ctx, "/a", "/a")
	if err != nil || p.ID != "aaaa2222" || p.DirKey != "/a" {
		t.Errorf("LatestParentInDir(/a) = %+v, %v", p, err)
	}
	if p, err := s.LatestParentInDir(ctx, "/x", "/a"); err != nil || p.ID != "aaaa1111" {
		t.Errorf("LatestParentInDir(key /x, dir /a) = %v, %v", p.ID, err)
	}
	if _, err := s.LatestParentInDir(ctx, "/c", "/c"); !errors.Is(err, ErrNotFound) {
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

	// SetChildActivity never lowers last_active.
	if err := s.SetChildActivity(ctx, "c2", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(time.Minute)
	if err := s.SetChildActivity(ctx, "c1", t2); err != nil {
		t.Fatal(err)
	}
	cs, _ = s.Children(ctx, "aaaa1111")
	if cs[0].ID != "c1" || !cs[0].LastActive.Equal(t2) || !cs[1].LastActive.Equal(t0) {
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

// TestFutureActivity (D23-2): a session file time from the future is
// capped when stored, and a row already holding one reads as its
// creation time and is replaced by the next real activity, so it cannot
// stay first for good.
func TestFutureActivity(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	t0 := now.Add(-time.Hour).Truncate(time.Millisecond)
	for _, id := range []string{"p1", "p2"} {
		if err := s.InsertParent(ctx, Parent{ID: id, Dir: "/" + id, CreatedAt: t0, LastActive: t0}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertChild(ctx, Child{ID: "c" + id, ParentID: id, Vendor: "claude", Cwd: "/", CreatedAt: t0, LastActive: t0}); err != nil {
			t.Fatal(err)
		}
	}
	// Capped on write.
	if err := s.SetChildActivity(ctx, "cp1", now.Add(100*365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Parents(ctx)
	if ps[0].ID != "p1" || ps[0].LastActive.After(time.Now().Add(FutureSlack)) {
		t.Errorf("capped write: first = %s at %v", ps[0].ID, ps[0].LastActive)
	}
	// A row poisoned before the cap existed.
	if _, err := s.DB().ExecContext(ctx, `UPDATE parents SET last_active = ? WHERE id = 'p1'`,
		millis(now.Add(50*365*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE children SET last_active = ? WHERE id = 'cp1'`,
		millis(now.Add(50*365*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.Parents(ctx)
	for _, p := range ps {
		if !p.LastActive.Equal(t0) {
			t.Errorf("parent %s reads as %v, want its creation time %v", p.ID, p.LastActive, t0)
		}
	}
	// Real activity on the other parent takes the lead, and on the
	// poisoned one heals it.
	if err := s.OnScreen(ctx, "p2", "cp2", "claude", now); err != nil {
		t.Fatal(err)
	}
	if ps, _ = s.Parents(ctx); ps[0].ID != "p2" {
		t.Errorf("after OnScreen p2, first = %s", ps[0].ID)
	}
	later := now.Add(time.Second)
	if err := s.SetChildActivity(ctx, "cp1", later); err != nil {
		t.Fatal(err)
	}
	if ps, _ = s.Parents(ctx); ps[0].ID != "p1" || !ps[0].LastActive.Equal(later.Truncate(time.Millisecond)) {
		t.Errorf("healed: first = %s at %v", ps[0].ID, ps[0].LastActive)
	}
	if cs, _ := s.Children(ctx, "p1"); !cs[0].LastActive.Equal(later.Truncate(time.Millisecond)) {
		t.Errorf("healed child at %v", cs[0].LastActive)
	}
}

// TestSetChildSessionIDLeased (D23-11): the id and its lease land in one
// update; a child that already has an id, or an id another child holds,
// changes nothing and leaves no lease.
func TestSetChildSessionIDLeased(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_000_000).UTC()
	if err := s.InsertParent(ctx, Parent{ID: "p1", Dir: "/a", CreatedAt: t0, LastActive: t0}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2"} {
		if err := s.InsertChild(ctx, Child{ID: id, ParentID: "p1", Vendor: "codex", Cwd: "/a", CreatedAt: t0, LastActive: t0}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.SetChildSessionIDLeased(ctx, "c1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, alive, _ := s.LeaseHolder(ctx, "codex", "t1"); !alive {
		t.Error("t1 recorded without its lease")
	}
	if _, err := s.SetChildSessionIDLeased(ctx, "c1", "t2"); !errors.Is(err, ErrSessionSet) {
		t.Errorf("a second id: %v", err)
	}
	if _, err := s.SetChildSessionIDLeased(ctx, "c2", "t1"); err == nil {
		t.Error("an id another child holds was recorded")
	}
	cs, _ := s.Children(ctx, "p1")
	for _, c := range cs {
		if c.ID == "c2" && c.VendorSessionID != "" {
			t.Errorf("c2 got id %q", c.VendorSessionID)
		}
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildFileSeen(ctx, "c2"); err != nil {
		t.Fatal(err)
	}
	cs, _ = s.Children(ctx, "p1")
	for _, c := range cs {
		if c.FileSeen != (c.ID == "c2") {
			t.Errorf("%s FileSeen = %v", c.ID, c.FileSeen)
		}
	}
}
