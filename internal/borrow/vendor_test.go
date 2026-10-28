package borrow

import (
	"context"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestVendorOverride (T6629): --vendor bypasses routing, even the
// parent's vendor, but still needs a connected vendor that supports the
// kind; --vendor both is research only and needs two vendors.
func TestVendorOverride(t *testing.T) {
	tests := []struct {
		name         string
		kind         adapter.Kind
		vendor       string
		parent       adapter.Vendor
		disconnected []adapter.Vendor
		want         adapter.Vendor
		wantErr      string
	}{
		{"pick codex over the routed grok", adapter.Image, "codex", "", nil, adapter.Codex, ""},
		{"the parent's own vendor", adapter.Image, "grok", adapter.Grok, nil, adapter.Grok, ""},
		{"not connected", adapter.Image, "codex", "", []adapter.Vendor{adapter.Codex}, "", "codex is not connected"},
		{"unsupported kind", adapter.Research, "codex", "", nil, "", "codex cannot take a research borrow"},
		{"claude cannot make images", adapter.Image, "claude", "", nil, "", "claude cannot take a image borrow"},
		{"unknown vendor", adapter.News, "gemini", "", nil, "", "unknown vendor"},
		{"both for research", adapter.Research, "both", "", nil, "", "only one vendor supports research right now (grok)"},
		{"both for news", adapter.News, "both", "", nil, "", "for research only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			for _, v := range tt.disconnected {
				w.connected[v] = false
			}
			w.scenario[adapter.Grok] = `{"json":{"result":"ok"}}`
			req := w.req(tt.kind, "x")
			req.Vendor, req.Parent = tt.vendor, tt.parent
			res, err := w.borrower().Run(context.Background(), req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if len(w.records()) != 0 {
					t.Error("ran a child")
				}
				return
			}
			if err != nil || len(res) != 1 || res[0].Vendor != tt.want {
				t.Fatalf("res %+v err %v", res, err)
			}
		})
	}
}

// TestBothRunsEach: when two vendors support research, both run.
func TestBothRunsEach(t *testing.T) {
	w := newWorld(t)
	b := w.borrower()
	// Stand in for a second research vendor: Grok under another name.
	inner := b.Adapter
	b.Adapter = func(v adapter.Vendor, cli CLI) adapter.Adapter {
		if v == adapter.Claude {
			return inner(adapter.Grok, cli)
		}
		return inner(v, cli)
	}
	b.Locate = func(context.Context, adapter.Vendor) (CLI, error) {
		return CLI{Path: w.fakes.Grok, Version: "1.0.70"}, nil
	}
	w.scenario[adapter.Grok] = `{"json":{"result":"ok"}}`
	req := w.req(adapter.Research, "x")
	req.Vendor = Both
	res, err := b.Run(context.Background(), req)
	if err != nil || len(res) != 2 || res[0].Vendor != adapter.Claude || res[1].Vendor != adapter.Grok {
		t.Fatalf("res %+v err %v", res, err)
	}
	if res[0].Status != "done" || res[1].Status != "done" {
		t.Errorf("statuses %s, %s", res[0].Status, res[1].Status)
	}
	if res[0].ID == res[1].ID || res[0].ReportPath == res[1].ReportPath {
		t.Error("the two borrows share an id")
	}
}
