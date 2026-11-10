package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// decideRec is one decision hook record.
func decideRec(id, event, code, cause, version string, ts time.Time) DiagRecord {
	attrs, _ := json.Marshal(map[string]string{"vendor_version": version})
	return DiagRecord{ID: id, Schema: 1, TS: ts, Level: "info", Component: "hook", Event: event, Code: code, Attrs: attrs,
		RunID: "r" + id, Vendor: "claude", CauseID: cause}
}

// TestDecideStatsAndPrune (T6976): doctor's sums per CLI version, and the
// pruning that keeps failures: a successful call goes after DecideKeep, and
// the age rule of Prune leaves the hook's records alone.
func TestDecideStatsAndPrune(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), DiagFileName)
	d, err := OpenDiag(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	for _, r := range []DiagRecord{
		// An old success, an old failed end, an old fallback, an old start with no end.
		decideRec("a1", DecideStarted, "", "", "2.1.300", old),
		decideRec("a2", DecideEnded, "decision.pre_approved", "a1", "2.1.300", old),
		decideRec("b1", DecideStarted, "", "", "2.1.300", old),
		decideRec("b2", DecideEnded, DecideFailedCode, "b1", "2.1.300", old),
		decideRec("c1", DecideFallback, "decision.fallback", "", "2.1.300", old),
		decideRec("d1", DecideStarted, "", "", "2.1.300", old),
		// Recent: a success, and a start still running.
		decideRec("e1", DecideStarted, "", "", "2.1.322", recent),
		decideRec("e2", DecideEnded, "decision.policy_match", "e1", "2.1.322", recent),
		decideRec("f1", DecideStarted, "", "", "2.1.322", now),
		// Another event, old.
		diagRec("g1", old),
	} {
		if err := d.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	stats := func() []DecideStat {
		v, err := OpenDiagView(ctx, path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = v.Close() }()
		s, err := v.DecideStats(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	want := []DecideStat{
		{Vendor: "claude", VendorVersion: "2.1.300", Calls: 3, Failed: 1, Fallbacks: 1, Unended: 1},
		{Vendor: "claude", VendorVersion: "2.1.322", Calls: 2},
	}
	if got := stats(); !slices.Equal(got, want) {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
	if want[0].Failures() != 3 {
		t.Errorf("failures = %d", want[0].Failures())
	}

	// Prune's age rule takes the other event and none of the hook's.
	if n, err := d.Prune(ctx, now.Add(-14*24*time.Hour), 0); err != nil || n != 1 {
		t.Errorf("Prune = %d, %v; want the one other record", n, err)
	}
	if n, err := d.PruneDecisions(ctx, now.Add(-DecideKeep)); err != nil || n != 2 {
		t.Errorf("PruneDecisions = %d, %v; want the old success's two records", n, err)
	}
	want[0].Calls = 2 // the success's start is gone; the failures stay
	if got := stats(); !slices.Equal(got, want) {
		t.Errorf("after pruning: %+v, want %+v", got, want)
	}
}
