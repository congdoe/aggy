package limits

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/router"
	"github.com/congdoe/aggy/internal/store"
)

var now = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func TestSignalUntil(t *testing.T) {
	tests := []struct {
		name string
		sig  Signal
		want time.Time
	}{
		{"hook ignores any reset", Signal{Until: now.Add(48 * time.Hour), Via: ViaHook}, now.Add(HintWindow)},
		{"unknown", Signal{Via: ViaRollout}, now.Add(Window)},
		{"past", Signal{Until: now.Add(-time.Minute), Via: ViaRollout}, now.Add(Window)},
		{"kept", Signal{Until: now.Add(3 * time.Hour), Via: ViaRollout}, now.Add(3 * time.Hour)},
		{"capped", Signal{Until: now.Add(400 * 24 * time.Hour), Via: ViaBorrow}, now.Add(Max)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sig.until(now); !got.Equal(tt.want) {
				t.Errorf("until = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFromHook(t *testing.T) {
	tests := []struct {
		name    string
		v       adapter.Vendor
		event   string
		payload string
		want    bool
	}{
		{"claude rate_limit", adapter.Claude, "StopFailure", `{"error":"rate_limit"}`, true},
		{"grok rate_limit", adapter.Grok, "StopFailure", `{"error":"rate_limit"}`, true},
		{"claude other error", adapter.Claude, "StopFailure", `{"error":"overloaded"}`, false},
		{"not StopFailure", adapter.Claude, "Stop", `{"error":"rate_limit"}`, false},
		{"codex has no failure hook", adapter.Codex, "StopFailure", `{"error":"rate_limit"}`, false},
		{"not json", adapter.Claude, "StopFailure", `x`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, ok := FromHook(tt.v, tt.event, []byte(tt.payload))
			if ok != tt.want || ok && (sig.Vendor != tt.v || sig.Via != ViaHook || !sig.Until.IsZero()) {
				t.Errorf("FromHook = %+v %v, want %v", sig, ok, tt.want)
			}
		})
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", t.TempDir())
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStateOverStore(t *testing.T) {
	type rec struct {
		sig Signal
		at  time.Time
	}
	tests := []struct {
		name    string
		recs    []rec
		v       adapter.Vendor
		want    router.Limit
		limited bool
	}{
		{"none", nil, adapter.Codex, router.Limit{}, false},
		{"other vendor", []rec{{Signal{Vendor: adapter.Grok, Via: ViaBorrow}, now}}, adapter.Codex, router.Limit{}, false},
		{"rollout", []rec{{Signal{Vendor: adapter.Codex, Until: now.Add(2 * time.Hour), Via: ViaRollout}, now}},
			adapter.Codex, router.Limit{Until: now.Add(2 * time.Hour)}, true},
		{"expired", []rec{{Signal{Vendor: adapter.Codex, Via: ViaRollout}, now.Add(-2 * time.Hour)}}, adapter.Codex, router.Limit{}, false},
		{"hook is a hint", []rec{{Signal{Vendor: adapter.Claude, Via: ViaHook}, now}},
			adapter.Claude, router.Limit{Until: now.Add(HintWindow), Hint: true}, true},
		{"confirmed wins over a later hint", []rec{
			{Signal{Vendor: adapter.Claude, Via: ViaBorrow}, now.Add(-30 * time.Minute)},
			{Signal{Vendor: adapter.Claude, Via: ViaHook}, now}},
			adapter.Claude, router.Limit{Until: now.Add(30 * time.Minute)}, true},
		{"latest until counts", []rec{
			{Signal{Vendor: adapter.Codex, Until: now.Add(5 * time.Hour), Via: ViaRollout}, now},
			{Signal{Vendor: adapter.Codex, Until: now.Add(2 * time.Hour), Via: ViaRollout}, now}},
			adapter.Codex, router.Limit{Until: now.Add(5 * time.Hour)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			for _, r := range tt.recs {
				if _, err := Record(context.Background(), s, r.sig, "", r.at); err != nil {
					t.Fatal(err)
				}
			}
			got, limited, err := State{Store: s}.Limited(context.Background(), tt.v, now)
			if err != nil || limited != tt.limited || !got.Until.Equal(tt.want.Until) || got.Hint != tt.want.Hint {
				t.Errorf("Limited = %+v %v %v, want %+v %v", got, limited, err, tt.want, tt.limited)
			}
		})
	}
}

// A row that claims more than its source allows (written by an older or
// broken aggy) is clamped on read.
func TestStateClampsRows(t *testing.T) {
	s := openStore(t)
	for _, d := range []map[string]any{
		{"vendor": "claude", "until": now.Add(30 * 24 * time.Hour).UnixMilli(), "via": ViaHook},
		{"vendor": "codex", "until": now.Add(30 * 24 * time.Hour).UnixMilli(), "via": ViaRollout},
	} {
		if err := s.AppendEvent(context.Background(), store.Event{At: now, Type: store.EventLimit, Detail: d}); err != nil {
			t.Fatal(err)
		}
	}
	st := State{Store: s}
	if got, _, _ := st.Limited(context.Background(), adapter.Claude, now); !got.Until.Equal(now.Add(HintWindow)) {
		t.Errorf("hook row until = %v, want %v", got.Until, now.Add(HintWindow))
	}
	if got, _, _ := st.Limited(context.Background(), adapter.Codex, now); !got.Until.Equal(now.Add(Max)) {
		t.Errorf("rollout row until = %v, want %v", got.Until, now.Add(Max))
	}
}

func line(ts, typ string, payload string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":%q,"payload":%s}`, ts, typ, payload) + "\n"
}

const (
	before = "2026-10-28T11:00:00.000Z"
	after  = "2026-10-28T11:59:00.000Z"
)

func TestCodexRollout(t *testing.T) {
	since := now.Add(-30 * time.Minute)
	reset := now.Add(3 * time.Hour).Unix()
	tc := func(used float64, resets int64) string {
		return line(after, "event_msg", fmt.Sprintf(`{"type":"token_count","rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"resets_at":%d},"secondary":{"used_percent":40,"window_minutes":10080,"resets_at":%d}}}`,
			used, resets, now.Add(100*time.Hour).Unix()))
	}
	limitDone := line(after, "event_msg", `{"type":"task_complete","error":{"codex_error_info":"usage_limit_exceeded"}}`)
	tests := []struct {
		name      string
		tail      string
		want      bool
		wantUntil time.Time
	}{
		{"structured with reset", tc(100, reset) + limitDone, true, time.Unix(reset, 0)},
		{"reset only from a full window", tc(80, reset) + limitDone, true, time.Time{}},
		{"reset already past", tc(100, now.Add(-time.Minute).Unix()) + limitDone, true, time.Time{}},
		{"text fallback in the error", line(after, "event_msg", `{"type":"turn_aborted","error":{"message":"You’ve hit your usage limit. Try again later."}}`), true, time.Time{}},
		{"straight apostrophe", line(after, "event_msg", `{"type":"task_complete","error":{"message":"You've hit your usage limit."}}`), true, time.Time{}},
		{"text a model wrote is ignored", line(after, "event_msg", `{"type":"task_complete","last_agent_message":"You've hit your usage limit."}`) +
			line(after, "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"You've hit your usage limit"}]}`), false, time.Time{}},
		{"a later turn succeeded", limitDone + line(after, "event_msg", `{"type":"task_complete"}`), false, time.Time{}},
		{"old limit before launch", line(before, "event_msg", `{"type":"task_complete","error":{"codex_error_info":"usage_limit_exceeded"}}`), false, time.Time{}},
		{"text inside another class is not a limit", line(after, "event_msg", `{"type":"task_complete","error":{"codex_error_info":"sandbox_error","message":"stdout: You've hit your usage limit."}}`), false, time.Time{}},
		{"text outside message is not a limit", line(after, "event_msg", `{"type":"task_complete","error":{"details":"You've hit your usage limit."}}`), false, time.Time{}},
		{"limit as an object variant", line(after, "event_msg", `{"type":"task_complete","error":{"codex_error_info":{"usage_limit_exceeded":{}}}}`), true, time.Time{}},
		{"other error", line(after, "event_msg", `{"type":"task_complete","error":{"codex_error_info":"context_window_exceeded"}}`), false, time.Time{}},
		{"garbage", "not json\n{\n", false, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, _, ok := CodexRollout([]byte(tt.tail), since, now)
			if ok != tt.want || !sig.Until.Equal(tt.wantUntil) || ok && (sig.Vendor != adapter.Codex || sig.Via != ViaRollout) {
				t.Errorf("CodexRollout = %+v %v, want %v until %v", sig, ok, tt.want, tt.wantUntil)
			}
		})
	}
}

func TestCodexWatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sessions := t.TempDir()
	start := time.Now().Add(-time.Minute)
	day := filepath.Join(sessions, start.Format("2006"), start.Format("01"), start.Format("02"))
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "0199a213-81c0-7800-8aa1-bbab2a035a53"
	path := filepath.Join(day, "rollout-2026-10-28T12-00-00-"+id+".jsonl")
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	limit := line(stamp, "event_msg", `{"type":"task_complete","error":{"codex_error_info":"usage_limit_exceeded"}}`)
	var got []Signal
	w := &CodexWatch{SessionsDir: sessions, Start: start, Found: func(s Signal) { got = append(got, s) }}

	write := func(s string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	write(line(stamp, "event_msg", `{"type":"task_complete"}`))
	w.Poll(time.Now())
	write(limit)
	w.Poll(time.Now())
	w.Poll(time.Now()) // unchanged: not read again
	write(line(stamp, "event_msg", `{"type":"token_count"}`))
	w.Poll(time.Now()) // same limit event: not reported again
	if len(got) != 1 {
		t.Fatalf("found %d limits, want 1: %+v", len(got), got)
	}

	// A symlinked rollout is never read.
	link := filepath.Join(day, "rollout-2026-10-28T12-00-01-"+"0199a213-81c0-7800-8aa1-bbab2a035a54"+".jsonl")
	target := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(target, []byte(limit), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	w.Poll(time.Now())
	if len(got) != 1 {
		t.Errorf("a symlinked rollout was read: %+v", got)
	}
}

// F27-2: a row written while the clock ran ahead is clamped from now
// too, not only from its own time.
func TestStateClampsFutureRows(t *testing.T) {
	s := openStore(t)
	ahead := now.Add(60 * 24 * time.Hour)
	for _, d := range []map[string]any{
		{"vendor": "claude", "until": ahead.Add(time.Hour).UnixMilli(), "via": ViaHook},
		{"vendor": "codex", "until": ahead.Add(time.Hour).UnixMilli(), "via": ViaRollout},
	} {
		if err := s.AppendEvent(context.Background(), store.Event{At: ahead, Type: store.EventLimit, Detail: d}); err != nil {
			t.Fatal(err)
		}
	}
	st := State{Store: s}
	if got, _, _ := st.Limited(context.Background(), adapter.Claude, now); !got.Until.Equal(now.Add(HintWindow)) {
		t.Errorf("hook row until = %v, want %v", got.Until, now.Add(HintWindow))
	}
	if got, _, _ := st.Limited(context.Background(), adapter.Codex, now); !got.Until.Equal(now.Add(Max)) {
		t.Errorf("rollout row until = %v, want %v", got.Until, now.Add(Max))
	}
}

// F27-3: hook rows, however many and however new, never push a
// confirmed limit out of the rows read.
func TestHintsCannotHideConfirmedLimit(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	weekly := now.Add(6 * 24 * time.Hour)
	if _, err := Record(ctx, s, Signal{Vendor: adapter.Codex, Until: weekly, Via: ViaRollout}, "", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRows+5; i++ {
		for _, v := range []adapter.Vendor{adapter.Codex, adapter.Claude} {
			if _, err := Record(ctx, s, Signal{Vendor: v, Via: ViaHook}, "", now.Add(-time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, limited, err := State{Store: s}.Limited(ctx, adapter.Codex, now)
	if err != nil || !limited || got.Hint || !got.Until.Equal(weekly) {
		t.Errorf("Limited = %+v %v %v, want the confirmed weekly limit", got, limited, err)
	}
}
