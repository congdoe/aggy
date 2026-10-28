package store

import (
	"context"
	"testing"
	"time"
)

// TestMarkRedone (T6701): which borrows count as redone.
func TestMarkRedone(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	row := func(kind, vendor, outcome, hash, parent string, start, end int) statRow {
		return statRow{kind: kind, vendor: vendor, outcome: outcome, taskHash: hash, parent: parent, start: at(start), end: at(end)}
	}
	rejected := row("edit", "codex", "ok", "h1", "p", 0, 5)
	rejected.rejected = true
	pairA, pairB := row("research", "grok", "ok", "h", "p", 0, 5), row("research", "claude", "ok", "h", "p", 5, 9)
	pairA.group, pairB.group = "g", "g"
	tests := []struct {
		name string
		rows []statRow
		want []bool
	}{
		{"same question, another vendor", []statRow{row("research", "grok", "ok", "h", "p", 0, 5), row("research", "codex", "ok", "h", "p", 10, 15)}, []bool{true, false}},
		{"same question, same vendor", []statRow{row("news", "grok", "ok", "h", "p", 0, 5), row("news", "grok", "ok", "h", "p", 6, 8)}, []bool{true, false}},
		{"chain: each attempt but the last", []statRow{row("news", "grok", "ok", "h", "p", 0, 1), row("news", "codex", "ok", "h", "p", 2, 3), row("news", "claude", "ok", "h", "p", 4, 5)}, []bool{true, true, false}},
		{"another question", []statRow{row("news", "grok", "ok", "h", "p", 0, 5), row("news", "grok", "ok", "h2", "p", 6, 8)}, []bool{false, false}},
		{"another parent", []statRow{row("news", "grok", "ok", "h", "p", 0, 5), row("news", "grok", "ok", "h", "q", 6, 8)}, []bool{false, false}},
		{"another kind", []statRow{row("news", "grok", "ok", "h", "p", 0, 5), row("research", "grok", "ok", "h", "p", 6, 8)}, []bool{false, false}},
		{"started before the first ended", []statRow{row("news", "grok", "ok", "h", "p", 0, 5), row("news", "codex", "ok", "h", "p", 4, 8)}, []bool{false, false}},
		{"past the window", []statRow{row("news", "grok", "ok", "h", "p", 0, 5), row("news", "codex", "ok", "h", "p", 5+24*60+1, 5+24*60+2)}, []bool{false, false}},
		{"at the window's edge", []statRow{row("news", "grok", "ok", "h", "p", 0, 5), row("news", "codex", "ok", "h", "p", 5+24*60, 5+24*60+2)}, []bool{true, false}},
		{"a failed borrow is not redone", []statRow{row("news", "grok", "rate_limit", "h", "p", 0, 5), row("news", "codex", "ok", "h", "p", 6, 8)}, []bool{false, false}},
		{"no parent left", []statRow{row("news", "grok", "ok", "h", "", 0, 5), row("news", "codex", "ok", "h", "", 6, 8)}, []bool{false, false}},
		{"no task hash", []statRow{row("news", "grok", "ok", "", "p", 0, 5), row("news", "codex", "ok", "", "p", 6, 8)}, []bool{false, false}},
		{"one --vendor both run", []statRow{pairA, pairB}, []bool{false, false}},
		{"a later run after a --vendor both run", []statRow{pairA, pairB, row("research", "grok", "ok", "h", "p", 20, 25)}, []bool{true, true, false}},
		{"rejected edit, then another edit", []statRow{rejected, row("edit", "codex", "ok", "h2", "p", 7, 9)}, []bool{true, false}},
		{"rejected edit, nothing after", []statRow{rejected}, []bool{false}},
		{"accepted edit, then another edit", []statRow{row("edit", "codex", "ok", "h1", "p", 0, 5), row("edit", "codex", "ok", "h2", "p", 7, 9)}, []bool{false, false}},
		{"a 24-hour borrow, then a re-ask", []statRow{row("news", "grok", "ok", "h", "p", 0, 24*60), row("news", "codex", "ok", "h", "p", 25*60, 25*60+1)}, []bool{true, false}},
		{"an end past any budget stops the scan", []statRow{row("news", "grok", "ok", "h", "p", 0, 10*24*60), row("news", "codex", "ok", "h", "p", 10*24*60+1, 10*24*60+2)}, []bool{false, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := markRedone(tt.rows, RedoWindow)
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("row %d: redone = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestBorrowStats (T6701): finished borrows per kind and vendor, with
// the redo rule applied to stored rows, a --vendor both group read from
// the borrow events, and a running borrow left out.
func TestBorrowStats(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	if err := s.InsertParent(ctx, Parent{ID: "p", Dir: "/repo", CreatedAt: t0, LastActive: t0}); err != nil {
		t.Fatal(err)
	}
	type b struct {
		id, kind, vendor, hash, status, outcome, group string
		tokens                                         int64
		start, minutes                                 int
	}
	borrows := []b{
		{"b1", "research", "grok", "h", "done", "ok", "", 100, 0, 2},
		{"b2", "research", "grok", "h", "done", "ok", "", 300, 10, 4}, // redoes b1
		{"b3", "research", "grok", "k", "done", "ok", "g", 50, 20, 1},
		{"b4", "research", "codex", "k", "done", "ok", "g", 70, 21, 1}, // same both run as b3
		{"b5", "news", "grok", "n", "failed", "rate_limit", "", 0, 30, 1},
		{"b6", "news", "grok", "n", "running", "", "", 0, 40, 0},
	}
	for _, x := range borrows {
		start := t0.Add(time.Duration(x.start) * time.Minute)
		lease, err := s.StartBorrow(ctx,
			Child{ID: "c" + x.id, ParentID: "p", Vendor: x.vendor, Kind: x.kind, CreatedAt: start, LastActive: start},
			Borrow{ID: x.id, Kind: x.kind, Vendor: x.vendor, TaskHash: x.hash, StartedAt: start}, BorrowCaps{})
		if err != nil {
			t.Fatal(err)
		}
		if x.status == "running" {
			continue
		}
		end := start.Add(time.Duration(x.minutes) * time.Minute)
		if err := lease.Finish(ctx, BorrowEnd{Status: x.status, Outcome: x.outcome, Tokens: x.tokens, EndedAt: end}); err != nil {
			t.Fatal(err)
		}
		d := map[string]any{"borrow": x.id}
		if x.group != "" {
			d["group"] = x.group
		}
		if err := s.AppendEvent(ctx, Event{At: end, ParentID: "p", ChildID: "c" + x.id, Type: EventBorrow, Detail: d}); err != nil {
			t.Fatal(err)
		}
	}
	want := []BorrowStat{
		{Kind: "news", Vendor: "grok", Runs: 1},
		{Kind: "research", Vendor: "codex", Runs: 1, OK: 1, OKTokens: 70, OKTime: time.Minute},
		{Kind: "research", Vendor: "grok", Runs: 3, OK: 3, Redone: 1, OKTokens: 450, OKTime: 7 * time.Minute},
	}
	check := func(name string, s *Store) {
		got, err := s.BorrowStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %+v", name, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: row %d = %+v, want %+v", name, i, got[i], want[i])
			}
		}
	}
	check("read-write", s)
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	check("read-only", ro)
}
