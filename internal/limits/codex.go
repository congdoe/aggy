package limits

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/transcript"
)

// Codex has no failure hook, so its limit is read from the rollout file
// Codex writes (docs/verification.md, "Codex limits (T6617)"; codex-rs
// 0.164.0, source only):
//
//   - an event_msg line whose payload is task_complete (or turn_aborted)
//     with error.codex_error_info "usage_limit_exceeded"
//     (protocol.rs#L2273, #L2195, #L4367; that this field is set on the
//     limit path is inferred);
//   - event_msg token_count lines, whose rate_limits.primary and
//     .secondary hold used_percent and resets_at in unix seconds
//     (protocol.rs#L2472, #L2478, #L2521), for the quota named by
//     rate_limits.limit_id (see CodexQuotas);
//   - turn_context lines, whose payload.model is the session's model slug
//     (protocol.rs#L3451; history/src/rollout_payload.rs#L32, #L51; one
//     such line was read live, docs/verification.md "Live runs");
//   - as a fallback, the CLI's text "You've hit your usage limit"
//     (protocol/src/error.rs#L719), looked for only inside such an
//     event's error, never in a message a model wrote.
//
// TODO(T6617): no real limit has been captured live; these lines are
// source-checked only.

// codexLimitInfo is the codex_error_info value of a usage limit.
const codexLimitInfo = "usage_limit_exceeded"

// codexLimitTexts are codex.LimitText with a straight and a curly
// apostrophe; verification.md records the curly one.
var codexLimitTexts = []string{codex.LimitText, strings.Replace(codex.LimitText, "'", "’", 1)}

// RolloutTail bounds what is read from the end of a rollout: the events
// that matter are the last few lines.
const RolloutTail = 256 << 10

// CodexRollout reads a rollout's tail. It reports a limit when the last
// task_complete or turn_aborted event in it ended on a usage limit and
// was written after since (an unreadable timestamp counts); at is that
// event's timestamp as written. Until comes from a rate-limit window at
// 100% or more among the session's quotas (CodexQuotas), when that is
// after now; else zero (unknown). When the ordinary "codex" quota has a
// full window, Until is its latest reset: that is the quota Codex's TUI
// treats as limiting (see CodexQuotas). A model quota's reset counts only
// when the ordinary quota has no full window, so a full weekly model
// quota cannot hold Codex for a week past the ordinary reset (L49-2). A
// window of another quota never gives the reset time, even at 100%.
func CodexRollout(tail []byte, since, now time.Time) (sig Signal, at string, ok bool) {
	var limited bool
	for _, line := range bytes.Split(tail, []byte{'\n'}) {
		var l struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type  string          `json:"type"`
				Error json.RawMessage `json:"error"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "event_msg" {
			continue
		}
		switch l.Payload.Type {
		case "task_complete", "turn_aborted":
			ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
			limited = codexLimitError(l.Payload.Error) && (err != nil || !ts.Before(since))
			at = l.Timestamp
		}
	}
	if !limited {
		return Signal{}, "", false
	}
	var ordinary, model time.Time
	ordinaryFull := false
	for _, q := range CodexQuotas(tail) {
		for _, w := range []*CodexWindow{q.Primary, q.Secondary} {
			if w == nil || w.UsedPercent < 100 {
				continue
			}
			var reset time.Time
			if w.ResetsAt > 0 {
				reset = time.Unix(w.ResetsAt, 0)
			}
			if q.ID == codexOrdinary {
				ordinaryFull = true
				ordinary = later(ordinary, reset)
			} else {
				model = later(model, reset)
			}
		}
	}
	until := model
	if ordinaryFull {
		until = ordinary
	}
	if !until.After(now) {
		until = time.Time{}
	}
	return Signal{Vendor: adapter.Codex, Until: until, Via: ViaRollout}, at, true
}

// CodexWindow is one rate-limit window of a Codex quota
// (RateLimitWindow, protocol.rs#L2521-L2530).
type CodexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"` // unix seconds
}

// CodexQuota is the latest snapshot of one Codex quota in a rollout.
type CodexQuota struct {
	ID        string       `json:"limit_id"`   // normalized; "codex" is the ordinary quota
	Name      string       `json:"limit_name"` // as written
	Primary   *CodexWindow `json:"primary"`
	Secondary *CodexWindow `json:"secondary"`
	At        string       `json:"-"` // the token_count line's timestamp as written
}

// codexOrdinary is the limit_id of the ordinary quota every session
// draws on.
const codexOrdinary = "codex"

// CodexQuotas returns the latest snapshot of each quota in a rollout's
// tail that applies to the session, in the order each quota first
// appears. Codex keeps one RateLimitSnapshot per quota, keyed by
// limit_id (protocol.rs#L2478-L2491; codex-rs at 6aaac694):
//
//   - a missing limit_id is the ordinary "codex" quota
//     (core/src/state/session.rs#L446-L453), and ids are compared
//     lowercased with '-' read as '_' (codex-api/src/rate_limits.rs#L270);
//   - Codex's TUI picks buckets for its refresh-poll cadence only, as
//     limit_id == "codex" or limit_name == the current model, exactly and
//     case-sensitively (tui/src/chatwidget/rate_limits.rs#L196-L202).
//     Its usage warnings and its rate-limit-reached state key off the
//     "codex" limit alone, matched with eq_ignore_ascii_case
//     (on_rate_limit_snapshot_from, #L252-L265, #L325, #L350).
//
// So the ordinary quota is source-checked as the one that limits a
// session. That a model-scoped quota (limit_name == the model of the last
// turn_context in the tail) limits it too is an inference from that poll
// filter, unverified: TODO(T6617) checks it on a live usage limit.
// CodexRollout prefers the ordinary quota's reset for that reason.
//
// normal_model_slug is not matched: it is metadata of a quota alias,
// "never a replacement for the request model" (protocol.rs#L2481-L2483,
// tui/src/chatwidget/luna_reserve_model.rs#L14-L18). Any other quota, or
// a model-scoped one when the tail holds no turn_context, is left out:
// failing closed, its reset time is never taken for the session's.
func CodexQuotas(tail []byte) []CodexQuota {
	var model string
	var order []string
	latest := map[string]CodexQuota{}
	for _, line := range bytes.Split(tail, []byte{'\n'}) {
		var l struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type       string      `json:"type"`
				Model      string      `json:"model"`
				RateLimits *CodexQuota `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		switch {
		case l.Type == "turn_context":
			model = l.Payload.Model
		case l.Type == "event_msg" && l.Payload.Type == "token_count" && l.Payload.RateLimits != nil:
			q := *l.Payload.RateLimits
			q.ID = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(q.ID)), "-", "_")
			if q.ID == "" {
				q.ID = codexOrdinary
			}
			q.At = l.Timestamp
			if _, ok := latest[q.ID]; !ok {
				order = append(order, q.ID)
			}
			latest[q.ID] = q
		}
	}
	var out []CodexQuota
	for _, id := range order {
		if q := latest[id]; id == codexOrdinary || model != "" && q.Name == model {
			out = append(out, q)
		}
	}
	return out
}

// codexLimitError reports whether an event's error names a usage limit:
// codex_error_info is usage_limit_exceeded (a string, or an object whose
// variant is that key). Only when codex_error_info is absent is the
// error's message matched against the CLI's limit text: other classes
// (sandbox_error among them) carry command output a model controls.
func codexLimitError(raw json.RawMessage) bool {
	var e struct {
		Info    json.RawMessage `json:"codex_error_info"`
		Message string          `json:"message"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil {
		return false
	}
	if len(e.Info) > 0 && !bytes.Equal(e.Info, []byte("null")) {
		var info string
		if json.Unmarshal(e.Info, &info) == nil {
			return info == codexLimitInfo
		}
		var variant map[string]json.RawMessage
		if json.Unmarshal(e.Info, &variant) == nil {
			_, ok := variant[codexLimitInfo]
			return ok
		}
		return false
	}
	for _, t := range codexLimitTexts {
		if strings.Contains(e.Message, t) {
			return true
		}
	}
	return false
}

// CodexWatch polls Codex's sessions folder for rollouts written since
// Start and reports each new limit in them.
//
// It reads only rollouts in the date folders a session started at Start
// could use (codex.RolloutDirs), only regular files that grew since the
// last look, and only their last RolloutTail bytes, through
// transcript.ReadTail (no symlinks, the opened file must be the one
// checked). A Codex limit is the account's, so any rollout of this
// account counts, including another terminal's.
//
// shortcut: a resumed session that writes to an older date folder is not
// watched; T6667 can pass the index's session id when it needs that.
type CodexWatch struct {
	SessionsDir string
	Start       time.Time
	// Found gets each new limit.
	Found func(Signal)

	seen map[string]int64  // path: size at the last read
	last map[string]string // path: the limit already reported
}

// Poll looks once.
func (w *CodexWatch) Poll(now time.Time) {
	if w.seen == nil {
		w.seen, w.last = map[string]int64{}, map[string]string{}
	}
	for _, dir := range codex.RolloutDirs(w.SessionsDir, w.Start, now) {
		es, err := safefs.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range es {
			if _, ok := codex.RolloutThreadID(e.Name()); !ok || !e.Type().IsRegular() {
				continue
			}
			fi, err := e.Info()
			if err != nil || fi.ModTime().Before(w.Start) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if w.seen[p] == fi.Size() {
				continue
			}
			w.seen[p] = fi.Size()
			tail, err := transcript.ReadTail(w.SessionsDir, p, RolloutTail)
			if err != nil {
				continue
			}
			sig, at, ok := CodexRollout(tail, w.Start, now)
			if !ok {
				// The near-limit warning (T6699) reads CodexAllowance
				// instead: the account's newest rollout, any age.
				delete(w.last, p)
				continue
			}
			// One report per limit event; a later write to the file that
			// leaves the same last event (say, a token_count) is no new limit.
			key := at + "|" + sig.Until.String()
			if w.last[p] == key {
				continue
			}
			w.last[p] = key
			w.Found(sig)
		}
	}
}

// allowanceTries bounds the rollouts CodexAllowance reads: a session that
// has not finished a turn has written no rate limits yet.
const allowanceTries = 5

// CodexAllowance returns the session quotas (CodexQuotas) of the newest
// Codex rollout under sessionsDir that holds any, among rollouts written
// in the last Max: what Codex last wrote about this account's limits. It
// reads as CodexWatch does: date folders only, regular rollout files, and
// only their last RolloutTail bytes through transcript.ReadTail, newest
// first, at most allowanceTries of them. Nil means Codex wrote nothing
// aggy can use.
func CodexAllowance(sessionsDir string, now time.Time) []CodexQuota {
	type rollout struct {
		path string
		mod  time.Time
	}
	var found []rollout
	seen := map[string]bool{}
	// One day ahead too: which clock names the date folder is not recorded.
	for d := -1; d <= int(Max/(24*time.Hour)); d++ {
		t := now.Add(-time.Duration(d) * 24 * time.Hour)
		for _, u := range []time.Time{t.Local(), t.UTC()} {
			dir := filepath.Join(sessionsDir, u.Format("2006"), u.Format("01"), u.Format("02"))
			if seen[dir] {
				continue
			}
			seen[dir] = true
			es, err := safefs.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range es {
				if _, ok := codex.RolloutThreadID(e.Name()); !ok || !e.Type().IsRegular() {
					continue
				}
				if fi, err := e.Info(); err == nil && fi.ModTime().After(now.Add(-Max)) {
					found = append(found, rollout{filepath.Join(dir, e.Name()), fi.ModTime()})
				}
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].mod.Equal(found[j].mod) {
			return found[i].mod.After(found[j].mod)
		}
		return found[i].path > found[j].path
	})
	for i, r := range found {
		if i == allowanceTries {
			break
		}
		tail, err := transcript.ReadTail(sessionsDir, r.path, RolloutTail)
		if err != nil {
			continue
		}
		if q := CodexQuotas(tail); len(q) > 0 {
			return q
		}
	}
	return nil
}

// CodexUsed returns the largest share used, 0 to 1, of the ordinary
// quota's windows whose reset is still ahead of now. Only the ordinary
// quota counts: Codex's own TUI warns off it alone (see CodexQuotas). A
// window with no reset time, or one already past, says nothing about now
// and is skipped; ok is false when no window is left. The near-limit
// warning (T6699) and aggy usage share it.
func CodexUsed(quotas []CodexQuota, now time.Time) (used float64, ok bool) {
	for _, q := range quotas {
		if q.ID != codexOrdinary {
			continue
		}
		for _, w := range []*CodexWindow{q.Primary, q.Secondary} {
			if w == nil || w.ResetsAt <= 0 || !time.Unix(w.ResetsAt, 0).After(now) {
				continue
			}
			used, ok = max(used, min(max(w.UsedPercent/100, 0), 1)), true
		}
	}
	return used, ok
}

// Run polls every interval until stop closes.
func (w *CodexWatch) Run(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			w.Poll(now)
		}
	}
}
