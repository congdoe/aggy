package borrow

import (
	"context"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// TestBorrowLog (T6630): each borrow records kind, vendor, model,
// tokens, time, and outcome in its row and in a borrow event, with
// tokens parsed from the CLI's JSON.
func TestBorrowLog(t *testing.T) {
	tests := []struct {
		name     string
		vendor   adapter.Vendor
		kind     adapter.Kind
		model    string
		scenario string
		tokens   int64
		outcome  string
	}{
		{"grok news", adapter.Grok, adapter.News, "grok-4.7",
			`{"extra_flags":["--model"],"json":{"result":"ok","usage":{"input_tokens":120,"output_tokens":30}}}`, 150, "ok"},
		{"codex image", adapter.Codex, adapter.Image, "",
			`{"events":[{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}]}`, 15, "ok"},
		{"claude review", adapter.Claude, adapter.Review, "",
			`{"json":{"type":"result","result":"ok","usage":{"input_tokens":7,"output_tokens":3}}}`, 10, "ok"},
		{"grok limit", adapter.Grok, adapter.News, "", `{"json":{"error":"rate_limit"},"exit_code":1}`, 15, "rate_limit"}, // the fake's default usage: 10 in, 5 out
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			for _, v := range adapter.Vendors() {
				w.connected[v] = v == tt.vendor
			}
			switch tt.vendor {
			case adapter.Grok:
				w.cfg.Vendors.Grok.Model = tt.model
			}
			w.scenario[tt.vendor] = tt.scenario
			res, err := w.borrower().Run(context.Background(), w.req(tt.kind, "x"))
			if err != nil {
				t.Fatal(err)
			}
			r := res[0]
			if r.Tokens != tt.tokens || r.Outcome != tt.outcome || r.Model != tt.model {
				t.Errorf("result %+v", r)
			}
			row, err := w.store.GetBorrow(context.Background(), r.ID)
			if err != nil || row.Tokens != tt.tokens || row.Outcome != tt.outcome || row.Model != tt.model ||
				row.Kind != string(tt.kind) || row.Vendor != string(tt.vendor) || row.EndedAt.IsZero() || row.Budget.Minutes == 0 {
				t.Errorf("row %+v, %v", row, err)
			}
			evs, err := w.store.Events(context.Background(), store.EventFilter{})
			if err != nil || len(evs) != 1 {
				t.Fatalf("events %v %v", evs, err)
			}
			d := evs[0].Detail
			if evs[0].Type != store.EventBorrow || d["borrow"] != r.ID || d["kind"] != string(tt.kind) || d["vendor"] != string(tt.vendor) ||
				d["model"] != tt.model || d["tokens"] != float64(tt.tokens) || d["outcome"] != tt.outcome || d["ms"] == nil || evs[0].ChildID == "" {
				t.Errorf("event %+v", evs[0])
			}
		})
	}
}
