package store

import (
	"context"
	"testing"
	"time"
)

func TestVendorStatus(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	at := time.UnixMilli(1_700_000_000_000).UTC()
	steps := []VendorStatus{
		{"codex", "unknown", "0.164.0", at},
		{"claude", "not_connected", "", at},
		{"codex", "connected", "0.165.0", at.Add(time.Minute)}, // replaces the first
	}
	for _, v := range steps {
		if err := s.PutVendorStatus(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.VendorStatuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []VendorStatus{steps[1], steps[2]}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for name, bad := range map[string]VendorStatus{
		"vendor":    {"gemini", "connected", "", at},
		"connected": {"grok", "yes", "", at},
	} {
		if err := s.PutVendorStatus(ctx, bad); err == nil {
			t.Errorf("bad %s accepted", name)
		}
	}
}
