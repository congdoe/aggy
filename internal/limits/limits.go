// Package limits records when a vendor hit its plan limit and reads it
// back for the router's "at its limit" skip rule.
//
// Signals come from three places (docs/design.md, section 11):
//
//   - a StopFailure hook whose error is rate_limit (Claude, Grok). Any
//     process the on-screen CLI starts can send a hook event, so this is
//     only a hint: it carries no reset time, marks the vendor for at most
//     HintWindow, and the router never lets it disable the last vendor
//     left (router.Limit.Hint).
//   - the Codex rollout file (codex.go), which Codex itself writes.
//   - a borrow whose headless child failed with rate_limit.
//
// Each signal is an append-only "limit" event in the store. Until is
// always set, clamped to at most Max ahead.
package limits

import (
	"context"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/router"
	"github.com/congdoe/aggy/internal/store"
)

// Where a signal came from.
const (
	ViaHook    = "hook"
	ViaRollout = "rollout"
	ViaBorrow  = "borrow"
)

const (
	// Window is how long a vendor counts as limited when its reset time
	// is unknown.
	Window = time.Hour
	// HintWindow is how long a hook-only signal counts: never longer,
	// whatever it claims.
	HintWindow = time.Hour
	// Max caps any reset time: a little over a week, so a weekly window
	// fits and a bad or hostile value cannot block a vendor for longer.
	Max = 8 * 24 * time.Hour
	// Skew is how far past now a row's own time may be and still count.
	// A row from further ahead was written while the clock ran ahead, and
	// clamping it from now would hold the vendor on every read (F31-1).
	Skew = 5 * time.Minute
)

// Signal is one report of a vendor at its limit.
type Signal struct {
	Vendor adapter.Vendor
	// Until is the vendor's reset time, or zero when unknown.
	Until time.Time
	Via   string
}

// until returns when aggy expects the vendor back: Until within
// (now, now+Max], else now+Window. A hook gets HintWindow.
func (s Signal) until(now time.Time) time.Time {
	switch {
	case s.Via == ViaHook:
		return now.Add(HintWindow)
	case !s.Until.After(now):
		return now.Add(Window)
	case s.Until.After(now.Add(Max)):
		return now.Add(Max)
	}
	return s.Until
}

// FromHook returns the signal in a forwarded hook event of vendor v: a
// StopFailure whose error is rate_limit (Claude and Grok; Codex has no
// failure hook). docs/verification.md records both payloads.
func FromHook(v adapter.Vendor, event string, payload []byte) (Signal, bool) {
	if event != "StopFailure" || v == adapter.Codex {
		return Signal{}, false
	}
	if adapter.ClassifyName(hook.Parse(v, payload).Error) != adapter.ErrRateLimit {
		return Signal{}, false
	}
	return Signal{Vendor: v, Via: ViaHook}, true
}

// Appender writes an event. *store.Store implements it.
type Appender interface {
	AppendEvent(ctx context.Context, e store.Event) error
}

// Record appends sig as a limit event under parentID ("" for none) and
// returns the time it records the vendor back.
func Record(ctx context.Context, a Appender, sig Signal, parentID string, now time.Time) (time.Time, error) {
	until := sig.until(now)
	return until, a.AppendEvent(ctx, store.Event{At: now, ParentID: parentID, Type: store.EventLimit,
		Detail: map[string]any{"vendor": string(sig.Vendor), "until": until.UnixMilli(), "via": sig.Via}})
}

// Reader reads limit events. *store.Store implements it.
type Reader interface {
	LimitEvents(ctx context.Context, vendor string, hook bool, since time.Time, limit int) ([]store.Event, error)
}

// maxRows bounds the limit events read per vendor and kind of source.
const maxRows = 500

// State is router.LimitState over the store's limit events.
type State struct{ Store Reader }

// Limited reports v limited while a limit event's until is after now. A
// confirmed signal (rollout, borrow) wins over a hook-only one; among
// several, the latest until counts. Each until is clamped again, both
// from the event's own time and from now, so no row holds a vendor longer
// than its source allows, and a row dated more than Skew ahead of now is
// skipped as stale.
func (s State) Limited(ctx context.Context, v adapter.Vendor, now time.Time) (router.Limit, bool, error) {
	sure, err := s.latest(ctx, v, false, now)
	if err != nil || !sure.IsZero() {
		return router.Limit{Until: sure}, !sure.IsZero(), err
	}
	hint, err := s.latest(ctx, v, true, now)
	if err != nil || hint.IsZero() {
		return router.Limit{}, false, err
	}
	return router.Limit{Until: hint, Hint: true}, true, nil
}

// latest returns the latest clamped until of v's hook or other limit
// events that is after now, or zero.
func (s State) latest(ctx context.Context, v adapter.Vendor, hook bool, now time.Time) (time.Time, error) {
	rows, err := s.Store.LimitEvents(ctx, string(v), hook, now.Add(-Max), maxRows)
	if err != nil {
		return time.Time{}, err
	}
	span := Max
	if hook {
		span = HintWindow
	}
	var out time.Time
	for _, e := range rows {
		ms, _ := e.Detail["until"].(float64)
		if ms <= 0 || e.At.After(now.Add(Skew)) {
			continue
		}
		until := time.UnixMilli(int64(ms))
		for _, limit := range []time.Time{e.At.Add(span), now.Add(span)} {
			if until.After(limit) {
				until = limit
			}
		}
		if until.After(now) {
			out = later(out, until)
		}
	}
	return out, nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
