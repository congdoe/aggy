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
