package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestChildBySession (T6965): the one child of a vendor that holds a session
// id, of any kind; another vendor's id, or none, is not found.
func TestChildBySession(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_000_000).UTC()
	if err := s.InsertParent(ctx, Parent{ID: "p1", Dir: "/a", CreatedAt: t0, LastActive: t0}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Child{
		{ID: "c1", Vendor: "claude", VendorSessionID: "s1"},
		{ID: "c2", Vendor: "codex", VendorSessionID: "s1"},
		{ID: "c3", Vendor: "claude", VendorSessionID: "s3", Kind: "review"},
		{ID: "c4", Vendor: "claude"},
	} {
		c.ParentID, c.Cwd, c.CreatedAt, c.LastActive = "p1", "/a", t0, t0
		if err := s.InsertChild(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		vendor, id, want string
	}{
		{"claude", "s1", "c1"},
		{"codex", "s1", "c2"},
		{"claude", "s3", "c3"},
		{"grok", "s1", ""},
		{"claude", "", ""},
		{"claude", "nope", ""},
		// PR #127 review: case is ignored.
		{"claude", "S1", "c1"},
		{"claude", "S3", "c3"},
	}
	for _, tt := range tests {
		c, err := s.ChildBySession(ctx, tt.vendor, tt.id)
		switch {
		case tt.want == "" && !errors.Is(err, ErrNotFound):
			t.Errorf("%s %q: %+v, %v; want not found", tt.vendor, tt.id, c, err)
		case tt.want != "" && (err != nil || c.ID != tt.want || c.ParentID != "p1"):
			t.Errorf("%s %q: %+v, %v; want %s", tt.vendor, tt.id, c, err, tt.want)
		}
	}
	// A second child cannot take an id another of the vendor holds.
	if err := s.InsertChild(ctx, Child{ID: "c5", ParentID: "p1", Vendor: "claude", VendorSessionID: "s1", Cwd: "/a", CreatedAt: t0, LastActive: t0}); err == nil {
		t.Error("a second child holds s1")
	}
}

// TestInsertParentChild (PR #127 review): a parent and its first child go in
// together; when another child of the vendor holds the session id, in any case,
// neither does and the error is ErrSessionHeld, not SQLite's text.
func TestInsertParentChild(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1_000_000).UTC()
	add := func(pid, cid, sid string) error {
		return s.InsertParentChild(ctx, Parent{ID: pid, Dir: "/a", CreatedAt: t0, LastActive: t0},
			Child{ID: cid, ParentID: pid, Vendor: "claude", VendorSessionID: sid, Cwd: "/a", CreatedAt: t0, LastActive: t0})
	}
	if err := add("p1", "c1", "s1"); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"s1", "S1"} {
		if err := add("p2", "c2", sid); !errors.Is(err, ErrSessionHeld) {
			t.Errorf("second adoption of %s: %v, want ErrSessionHeld", sid, err)
		}
		if _, err := s.GetParent(ctx, "p2"); !errors.Is(err, ErrNotFound) {
			t.Errorf("an empty parent was left behind for %s: %v", sid, err)
		}
	}
	if err := add("p3", "c3", "s3"); err != nil {
		t.Fatal(err)
	}
	// DeleteEmptyParent keeps a parent with a child and removes one without.
	if ok, err := s.DeleteEmptyParent(ctx, "p3"); err != nil || ok {
		t.Errorf("DeleteEmptyParent(p3 with a child) = %v, %v", ok, err)
	}
	if err := s.InsertParent(ctx, Parent{ID: "p4", Dir: "/a", CreatedAt: t0, LastActive: t0}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteEmptyParent(ctx, "p4"); err != nil || !ok {
		t.Errorf("DeleteEmptyParent(p4) = %v, %v", ok, err)
	}
}
