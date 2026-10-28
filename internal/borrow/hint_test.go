package borrow

import (
	"context"
	"errors"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/router"
)

// TestHintedVendorRetried (F27-1): a vendor skipped only for a hook's
// unconfirmed limit is tried when no other candidate can start; a
// confirmed limit still skips it.
func TestHintedVendorRetried(t *testing.T) {
	tests := []struct {
		name    string
		limit   router.Limit
		wantRun bool
	}{
		{"hint is retried", router.Limit{Hint: true}, true},
		{"confirmed limit still skips", router.Limit{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			// image routes codex, grok: codex cannot start (its version is
			// unverified), grok is limited.
			w.version[adapter.Codex] = "unreadable"
			w.limits = map[adapter.Vendor]router.Limit{adapter.Grok: tt.limit}
			res, err := w.borrower().Run(context.Background(), w.req(adapter.Image, "a cat"))
			if !tt.wantRun {
				var ns *NoStartError
				if !errors.As(err, &ns) || len(w.records()) != 0 {
					t.Fatalf("err = %v, records %d", err, len(w.records()))
				}
				return
			}
			if len(res) != 1 || res[0].Vendor != adapter.Grok || len(res[0].Tried) != 1 || res[0].Tried[0].Vendor != adapter.Codex {
				t.Fatalf("results = %+v, err %v", res, err)
			}
			if len(w.records()) != 1 {
				t.Errorf("records = %d, want 1", len(w.records()))
			}
		})
	}
}

// F31-2: with no vendor left out for a hint, a borrow no candidate can
// start does not ask the router again.
func TestNoHintNoSecondPass(t *testing.T) {
	w := newWorld(t)
	w.version[adapter.Codex] = "unreadable"
	w.version[adapter.Grok] = "unreadable"
	_, err := w.borrower().Run(context.Background(), w.req(adapter.Image, "a cat"))
	var ns *NoStartError
	if !errors.As(err, &ns) || w.chooses != 1 {
		t.Fatalf("err = %v, router asked %d times, want 1", err, w.chooses)
	}
}
