package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/limits"
	"github.com/congdoe/aggy/internal/router"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

func TestSessionLimitsHook(t *testing.T) {
	limit := supervisor.Request{Type: supervisor.ReqHookEvent, Vendor: "claude", Event: "StopFailure", Payload: []byte(`{"error":"rate_limit"}`)}
	tests := []struct {
		name string
		reqs []supervisor.Request
		want int
	}{
		{"one limit", []supervisor.Request{limit}, 1},
		{"a burst is recorded once", []supervisor.Request{limit, limit, limit}, 1},
		{"another vendor's event", []supervisor.Request{{Vendor: "grok", Event: "StopFailure", Payload: []byte(`{"error":"rate_limit"}`)}}, 0},
		{"another error", []supervisor.Request{{Vendor: "claude", Event: "StopFailure", Payload: []byte(`{"error":"overloaded"}`)}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []limits.Signal
			l := &sessionLimits{vendor: adapter.Claude, record: func(s limits.Signal) { got = append(got, s) }}
			for _, r := range tt.reqs {
				l.hook(r)
			}
			if len(got) != tt.want {
				t.Fatalf("recorded %d, want %d", len(got), tt.want)
			}
			for _, s := range got {
				if s.Via != limits.ViaHook || s.Vendor != adapter.Claude {
					t.Errorf("signal = %+v", s)
				}
			}
		})
	}
}

type appended []store.Event

func (a *appended) AppendEvent(_ context.Context, e store.Event) error {
	*a = append(*a, e)
	return nil
}

func TestRecordBorrowLimits(t *testing.T) {
	var a appended
	recordBorrowLimits(context.Background(), &a, []borrow.Result{
		{Vendor: adapter.Codex, Outcome: string(adapter.ErrRateLimit)},
		{Vendor: adapter.Grok, Outcome: "ok"},
		{Vendor: adapter.Claude, Outcome: string(adapter.ErrServer)},
	}, time.Now())
	if len(a) != 1 || a[0].Type != store.EventLimit || a[0].Detail["vendor"] != "codex" || a[0].Detail["via"] != limits.ViaBorrow {
		t.Errorf("events = %+v", a)
	}
}

type hintLimits struct{}

func (hintLimits) Limited(_ context.Context, v adapter.Vendor, _ time.Time) (router.Limit, bool, error) {
	return router.Limit{Hint: true}, v == adapter.Codex, nil
}

type connected struct{}

func (connected) VendorStatuses(context.Context) ([]store.VendorStatus, error) {
	return []store.VendorStatus{{Vendor: "claude", Connected: "connected"}, {Vendor: "codex", Connected: "connected"}, {Vendor: "grok", Connected: "connected"}}, nil
}

// A hook-only limit on the last vendor left is shown, not silent.
func TestBorrowRouterShowsWarnings(t *testing.T) {
	var warn bytes.Buffer
	b := borrowRouter{router.New(config.Default(), router.Deps{Statuses: connected{}, Limits: hintLimits{}}), &warn}
	got, err := b.Choose(context.Background(), borrow.RouteRequest{Kind: adapter.Image, Parent: adapter.Grok})
	if err != nil || len(got) != 1 || got[0] != adapter.Codex {
		t.Fatalf("Choose = %v %v", got, err)
	}
	if !strings.Contains(warn.String(), "aggy borrow: warning: codex: a hook reported its limit") {
		t.Errorf("warning = %q", warn.String())
	}
}
