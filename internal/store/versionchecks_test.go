package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestVersionChecks: a result is found only for its vendor, version, check,
// and stamp, and a new one replaces it.
func TestVersionChecks(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	c := VersionCheck{Vendor: "codex", Version: "0.165.0", Check: "selftest", Stamp: "s1", Reason: "it failed", At: at}
	if err := s.PutVersionCheck(ctx, c); err != nil {
		t.Fatal(err)
	}
	misses := []struct{ vendor, version, check, stamp string }{
		{"grok", "0.165.0", "selftest", "s1"},
		{"codex", "0.166.0", "selftest", "s1"},
		{"codex", "0.165.0", "features", "s1"},
		{"codex", "0.165.0", "selftest", "s2"},
	}
	for _, m := range misses {
		if _, err := s.GetVersionCheck(ctx, m.vendor, m.version, m.check, m.stamp); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v: err = %v, want not found", m, err)
		}
	}
	got, err := s.GetVersionCheck(ctx, "codex", "0.165.0", "selftest", "s1")
	if err != nil || got.Passed || got.Reason != "it failed" || !got.At.Equal(at) {
		t.Errorf("got %+v, %v", got, err)
	}
	c.Stamp, c.Passed, c.Reason = "s2", true, ""
	if err := s.PutVersionCheck(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVersionCheck(ctx, "codex", "0.165.0", "selftest", "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old stamp survived: %v", err)
	}
	if got, err := s.GetVersionCheck(ctx, "codex", "0.165.0", "selftest", "s2"); err != nil || !got.Passed {
		t.Errorf("got %+v, %v", got, err)
	}
}
