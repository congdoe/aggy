package cli

import (
	"context"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// TestRoIndex (T6770): the picker's reads share one read-only handle,
// opened once, and closed for good at exit.
func TestRoIndex(t *testing.T) {
	s := sessionStore(t)
	seedParent(t, s, "p1", t.TempDir(),
		store.Child{ID: "c1", Vendor: "grok", VendorSessionID: sidA},
		store.Child{ID: "c2", Vendor: "claude", VendorSessionID: sidB})
	opens := 0
	orig := openIndexRO
	t.Cleanup(func() { openIndexRO = orig })
	openIndexRO = func(ctx context.Context, path string) (*store.Store, error) {
		opens++
		return orig(ctx, path)
	}
	ro := &roIndex{}
	for range 5 {
		if got := childChoices(ro.get(), "p1", adapter.Grok, time.Now()); len(got) != 1 || got[0].ID != "c1" {
			t.Fatalf("children %+v", got)
		}
		_ = hasQuota(ro.get(), adapter.Grok)
	}
	if opens != 1 {
		t.Errorf("%d opens, want 1", opens)
	}
	h := ro.get()
	ro.close()
	if _, err := h.Children(context.Background(), "p1"); err == nil {
		t.Error("the handle still reads after close")
	}
	if ro.get() != nil || opens != 1 {
		t.Errorf("get after close: opened again (%d opens)", opens)
	}
}

// TestChildRecorded (T6770): the check before a switch's exit command
// finds only the parent's interactive child of that vendor.
func TestChildRecorded(t *testing.T) {
	s := sessionStore(t)
	seedParent(t, s, "p1", t.TempDir(),
		store.Child{ID: "c1", Vendor: "grok", VendorSessionID: sidA})
	tests := []struct {
		v    adapter.Vendor
		id   string
		want bool
	}{
		{adapter.Grok, "c1", true},
		{adapter.Grok, "c9", false},
		{adapter.Claude, "c1", false},
	}
	for _, tt := range tests {
		if got := childRecorded(s, "p1", tt.v, tt.id); got != tt.want {
			t.Errorf("%s/%s: %v, want %v", tt.v, tt.id, got, tt.want)
		}
	}
	if !childRecorded(nil, "p1", adapter.Grok, "c9") {
		t.Error("without an index the switch should go on")
	}
}
