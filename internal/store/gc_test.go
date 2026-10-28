//go:build unix

package store

import (
	"context"
	"os"
	"testing"
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
