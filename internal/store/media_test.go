package store

import (
	"context"
	"testing"
	"time"
)

// TestMediaSession (B15-10): only a session an image borrow recorded for
// the same repo counts; another repo's, a video's, or a news child's does
// not.
func TestMediaSession(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	for i, r := range []struct{ dir, kind, session string }{
		{"/repo", "image", "s-image"},
		{"/repo", "video", "s-video"},
		{"/repo", "news", "s-news"},
		{"/other", "image", "s-other"},
	} {
		id := string(rune('a' + i))
		p, err := s.ParentForDir(ctx, r.dir, "borrows", "p"+id, now)
		if err != nil {
			t.Fatal(err)
		}
		l, err := s.StartBorrow(ctx,
			Child{ID: "c" + id, ParentID: p.ID, Vendor: "codex", Kind: r.kind, Cwd: "/tmp/w", CreatedAt: now, LastActive: now},
			Borrow{ID: "b" + id, Kind: r.kind, Vendor: "codex", StartedAt: now}, BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Finish(ctx, BorrowEnd{Status: "done", Outcome: "ok", EndedAt: now, VendorSessionID: r.session}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		dir, vendor, kind, session string
		want                       bool
	}{
		{"/repo", "codex", "image", "s-image", true},
		{"/repo", "grok", "image", "s-image", false},
		{"/other", "codex", "image", "s-image", false},
		{"/repo", "codex", "image", "s-video", false},
		{"/repo", "codex", "image", "s-news", false},
		{"/repo", "codex", "image", "s-other", false},
		{"/repo", "codex", "image", "", false},
	} {
		if got, err := s.MediaSession(ctx, tt.dir, tt.vendor, tt.kind, tt.session); err != nil || got != tt.want {
			t.Errorf("MediaSession(%+v) = %v, %v", tt, got, err)
		}
	}
}
