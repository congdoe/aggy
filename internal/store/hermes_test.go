package store

import (
	"context"
	"testing"
	"time"
)

func TestHermesLaunches(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	at := time.UnixMilli(1_700_000_000_000).UTC()
	a := HermesLaunch{ParentID: "p1", ChildID: "c1", Profile: "aggy-0123456789ab", Root: "/r", Cwd: "/r/sub", Plan: "/plan", PlanSum: "sum", Token: "tok", Started: at}
	b := HermesLaunch{ParentID: "p2", ChildID: "c2", Profile: "aggy-0123456789ab", Root: "/r", Cwd: "/r", Token: "tok2", SessionID: "20261110_101500_a1b2c3", Started: at}
	other := HermesLaunch{ParentID: "p3", ChildID: "c3", Profile: "aggy-ba9876543210", Token: "t3", Started: at}
	var ids []int64
	for _, l := range []HermesLaunch{a, b, other} {
		id, err := s.PutHermesLaunch(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	got, err := s.HermesLaunches(ctx, a.Profile)
	if err != nil || len(got) != 2 {
		t.Fatalf("launches = %+v, %v", got, err)
	}
	a.ID, b.ID = ids[0], ids[1]
	if got[0] != a || got[1] != b {
		t.Fatalf("got %+v\nwant %+v %+v", got, a, b)
	}
	// Claimed once: a second hook naming another new session loses.
	if ok, err := s.ClaimHermesSession(ctx, a.ID, "", "20261110_101501_aaaaaa"); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if ok, _ := s.ClaimHermesSession(ctx, a.ID, "", "20261110_101502_bbbbbb"); ok {
		t.Fatal("a claimed launch was claimed again")
	}
	// A move: from the id it holds.
	if ok, _ := s.ClaimHermesSession(ctx, a.ID, "20261110_101501_aaaaaa", "20261110_101503_cccccc"); !ok {
		t.Fatal("a move was refused")
	}
	if err := s.DeleteHermesLaunch(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.HermesLaunches(ctx, a.Profile)
	if len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("after delete = %+v", got)
	}
}
