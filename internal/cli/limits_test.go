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
	"github.com/congdoe/aggy/internal/notify"
	"github.com/congdoe/aggy/internal/router"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

func TestSessionLimitsHook(t *testing.T) {
	limit := supervisor.Request{Type: supervisor.ReqHookEvent, Vendor: "claude", Event: "StopFailure", Payload: []byte(`{"error":"rate_limit"}`), FromSession: true}
	outside := limit
	outside.FromSession = false
	tests := []struct {
		name string
		reqs []supervisor.Request
		want int
	}{
		{"one limit", []supervisor.Request{limit}, 1},
		{"a burst is recorded once", []supervisor.Request{limit, limit, limit}, 1},
		{"from outside the CLI's process tree", []supervisor.Request{outside}, 0},
		{"another vendor's event", []supervisor.Request{{Vendor: "grok", Event: "StopFailure", Payload: []byte(`{"error":"rate_limit"}`), FromSession: true}}, 0},
		{"another error", []supervisor.Request{{Vendor: "claude", Event: "StopFailure", Payload: []byte(`{"error":"overloaded"}`), FromSession: true}}, 0},
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
	}, time.Now(), nil)
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

// A Notification hook from the vendor on screen shows aggy's own text,
// whatever the payload says.
func TestSessionWaitingNotification(t *testing.T) {
	shown := make(chan adapter.Command, 4)
	n := &notify.Notifier{GOOS: "linux", Path: "/usr/bin/notify-send", Every: time.Minute,
		Run: func(_ context.Context, c adapter.Command) (adapter.Output, error) {
			shown <- c
			return adapter.Output{}, nil
		}}
	l := &sessionLimits{vendor: adapter.Claude, notifier: n, record: func(limits.Signal) { t.Error("recorded a limit") }}
	l.hook(supervisor.Request{Vendor: "grok", Event: "Notification", Payload: []byte(`{}`)})
	l.hook(supervisor.Request{Vendor: "claude", Event: "Notification", Payload: []byte(`{"message":"<b>/Users/x/secret</b>"}`)})
	select {
	case c := <-shown:
		if body := c.Args[len(c.Args)-1]; body != "Claude is waiting for you." {
			t.Errorf("body = %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing shown")
	}
	select {
	case c := <-shown:
		t.Errorf("another vendor's event was shown: %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNotificationTexts(t *testing.T) {
	until := time.Date(2026, 10, 28, 15, 4, 0, 0, time.Local)
	for _, tt := range []struct{ got, want string }{
		{limitText(limits.Signal{Vendor: adapter.Codex, Via: limits.ViaRollout}, until), "Codex hit its usage limit; aggy expects it back about Oct 28 15:04."},
		{limitText(limits.Signal{Vendor: adapter.Claude, Via: limits.ViaHook}, until), "Claude reported a usage limit; aggy expects it back about Oct 28 15:04."},
		{borrowText(adapter.Image, adapter.Grok, "ok"), "The image borrow on Grok finished: ok."},
	} {
		if tt.got != tt.want {
			t.Errorf("text = %q, want %q", tt.got, tt.want)
		}
	}
}
