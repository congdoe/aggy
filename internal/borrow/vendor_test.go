package borrow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
		// X-3: --vendor goes through the router's skip rules.
		{"the parent's own vendor", adapter.Image, "grok", adapter.Grok, nil, "", "grok: the parent is on grok"},
		{"not connected", adapter.Image, "codex", "", []adapter.Vendor{adapter.Codex}, "", "codex: not connected"},
		{"unsupported kind", adapter.Research, "codex", "", nil, "", "codex: research: not supported by this CLI until T6638"},
		{"claude cannot make images", adapter.Image, "claude", "", nil, "", "claude: image: not supported"},
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
		return CLI{Path: w.fakes.Grok, Version: "1.0.70", Caps: []string{"sandbox", "kernel-sandbox"}}, nil
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
	// T6701: one --vendor both run is not a redo of itself; the same
	// question asked again later (case and spacing aside) redoes both.
	redone := func() map[string]int {
		st, err := w.store.BorrowStats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, s := range st {
			out[s.Vendor] = s.Redone
		}
		return out
	}
	if got := redone(); got["claude"] != 0 || got["grok"] != 0 {
		t.Errorf("after one both run, redone = %v", got)
	}
	w.now = w.now.Add(time.Hour)
	again := w.req(adapter.Research, "  X ")
	again.Vendor, again.Fresh = "grok", true
	if _, err := b.Run(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	if got := redone(); got["claude"] != 1 || got["grok"] != 1 {
		t.Errorf("after a re-ask, redone = %v", got)
	}
}

// TestFallThrough (X-1): candidates are tried in order. A vendor whose
// adapter refuses the kind, whose CLI is missing, or whose version the
// lockdown was not verified on is passed over, and reported; a run that
// started and failed is the result, with no second vendor.
func TestFallThrough(t *testing.T) {
	tests := []struct {
		name       string
		routing    string // research route
		missing    adapter.Vendor
		untested   adapter.Vendor
		grokFails  bool
		kind       adapter.Kind
		wantVendor adapter.Vendor
		wantTried  []string
		wantErr    string
		wantRuns   int
	}{
		{"adapter refuses", "codex,grok", "", "", false, adapter.Research, adapter.Grok, []string{"codex: research: not supported"}, "", 1},
		{"cli missing", "", adapter.Codex, "", false, adapter.Image, adapter.Grok, []string{"codex: not installed"}, "", 1},
		{"untested version", "", "", adapter.Codex, false, adapter.Image, adapter.Grok, []string{"pass --allow-untested"}, "", 1},
		{"every vendor refuses", "codex", "", "", false, adapter.Research, "", nil, "no vendor could start the research borrow: codex: research", 0},
		{"a started run that fails is final", "grok,codex", "", "", true, adapter.Image, adapter.Grok, nil, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			if tt.routing != "" {
				var vs []adapter.Vendor
				for _, v := range strings.Split(tt.routing, ",") {
					vs = append(vs, adapter.Vendor(v))
				}
				w.cfg.Routing.Research.Vendors = vs
				w.cfg.Routing.Image.Vendors = vs
			}
			if tt.untested != "" {
				w.version[tt.untested] = "9.9.9"
			}
			w.scenario[adapter.Grok] = `{"images":1,"json":{"result":"ok"}}`
			if tt.grokFails {
				w.scenario[adapter.Grok] = `{"json":{"error":"rate_limit"},"exit_code":1}`
			}
			b := w.borrower()
			locate := b.Locate
			b.Locate = func(ctx context.Context, v adapter.Vendor) (CLI, error) {
				if v == tt.missing {
					return CLI{}, errors.New("not installed")
				}
				return locate(ctx, v)
			}
			res, err := b.Run(context.Background(), w.req(tt.kind, "x"))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v", err)
				}
			} else {
				if err != nil || len(res) != 1 || res[0].Vendor != tt.wantVendor || len(res[0].Tried) != len(tt.wantTried) {
					t.Fatalf("res %+v err %v", res, err)
				}
				for i, want := range tt.wantTried {
					if !strings.Contains(res[0].Tried[i].Reason, want) {
						t.Errorf("tried[%d] = %q, want %q", i, res[0].Tried[i].Reason, want)
					}
				}
			}
			if n := len(w.records()); n != tt.wantRuns {
				t.Errorf("runs = %d, want %d", n, tt.wantRuns)
			}
		})
	}
}
