package borrow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// TestTurnLimits (T6622): each kind's turn budget reaches Grok as
// --max-turns; a kind with no turn budget passes none.
func TestTurnLimits(t *testing.T) {
	tests := []struct {
		kind adapter.Kind
		want []string
	}{
		{adapter.News, []string{"15"}},
		{adapter.Research, []string{"30"}},
		{adapter.Image, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			w := newWorld(t)
			w.connected[adapter.Codex] = false
			w.scenario[adapter.Grok] = `{"json":{"result":"ok"}}`
			if _, err := w.borrower().Run(context.Background(), w.req(tt.kind, "x")); err != nil {
				t.Fatal(err)
			}
			if got := w.records()[0].Flags["--max-turns"]; !slices.Equal(got, tt.want) {
				t.Errorf("--max-turns = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTimeLimitKills (T6622): past the time limit the child is killed
// and the borrow is recorded as killed with outcome timeout.
func TestTimeLimitKills(t *testing.T) {
	w := newWorld(t)
	w.timeout = time.Second
	w.scenario[adapter.Grok] = `{"sleep_ms":20000,"json":{"result":"late"}}`
	start := time.Now()
	res, err := w.borrower().Run(context.Background(), w.req(adapter.News, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the child was not killed at its limit (%s)", time.Since(start))
	}
	if res[0].Status != "killed" || res[0].Outcome != string(adapter.ErrTimeout) {
		t.Errorf("result %+v", res[0])
	}
	row, _ := w.store.GetBorrow(context.Background(), res[0].ID)
	if row.Status != "killed" || row.Outcome != "timeout" {
		t.Errorf("row %+v", row)
	}
}

// TestConcurrencyCap (T6622): with 2 Grok borrows running (in any
// process), a third is refused with a clear error and runs nothing.
func TestConcurrencyCap(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	p, err := w.store.ParentForDir(ctx, w.repo, "borrows", "p", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2"} {
		l, err := w.store.StartBorrow(ctx,
			store.Child{ID: "c" + id, ParentID: p.ID, Vendor: "grok", Kind: "news", Cwd: "/w", CreatedAt: now, LastActive: now},
			store.Borrow{ID: id, Kind: "news", Vendor: "grok", StartedAt: now}, Caps)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Finish(ctx, store.BorrowEnd{Status: "done", EndedAt: now}) }()
	}
	_, err = w.borrower().Run(ctx, w.req(adapter.News, "x"))
	if !errors.Is(err, store.ErrCap) || !strings.Contains(err.Error(), "2 grok borrows are already running") {
		t.Errorf("err = %v", err)
	}
	if len(w.records()) != 0 {
		t.Error("ran a child past the cap")
	}
}

// TestUntestedVersion (#10-5): a CLI outside the tested versions, or
// with an unreadable version, is refused unless AllowUntested.
func TestUntestedVersion(t *testing.T) {
	tests := []struct {
		version string
		allow   bool
		wantErr string
	}{
		{"1.0.70", false, ""},
		{"1.0.99", false, "pass --allow-untested"},
		{"", false, "could not parse the CLI version"},
		{"1.0.99", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			w := newWorld(t)
			w.version[adapter.Grok] = tt.version
			if tt.version == "" {
				w.version[adapter.Grok] = "unreadable"
			}
			w.scenario[adapter.Grok] = `{"json":{"result":"ok"}}`
			req := w.req(adapter.News, "x")
			req.AllowUntested = tt.allow
			_, err := w.borrower().Run(context.Background(), req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v", err)
			}
			if len(w.records()) != 0 {
				t.Error("ran an untested CLI")
			}
		})
	}
}
