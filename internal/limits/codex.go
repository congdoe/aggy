package limits

import (
	"bytes"
	"encoding/json"
	"path/filepath"
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
//     (protocol.rs#L2472, #L2478, #L2521);
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
// event's timestamp as written. Until is the latest resets_at of a
// rate-limit window at 100% or more in the last token_count, when that
// is after now; else zero (unknown).
func CodexRollout(tail []byte, since, now time.Time) (sig Signal, at string, ok bool) {
	var limited bool
	var until time.Time
	for _, line := range bytes.Split(tail, []byte{'\n'}) {
		var l struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type       string          `json:"type"`
				Error      json.RawMessage `json:"error"`
				RateLimits *struct {
					Primary, Secondary *struct {
						UsedPercent float64 `json:"used_percent"`
						ResetsAt    int64   `json:"resets_at"`
					}
				} `json:"rate_limits"`
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
		case "token_count":
			// TODO(T6741): RateLimitSnapshot also has limit_id and
			// normal_model_slug; a snapshot for another limit or model may
			// not be the one that stopped this session.
			if rl := l.Payload.RateLimits; rl != nil {
				until = time.Time{}
				for _, w := range []*struct {
					UsedPercent float64 `json:"used_percent"`
					ResetsAt    int64   `json:"resets_at"`
				}{rl.Primary, rl.Secondary} {
					if w != nil && w.UsedPercent >= 100 && w.ResetsAt > 0 {
						until = later(until, time.Unix(w.ResetsAt, 0))
					}
				}
			}
		}
	}
	if !limited {
		return Signal{}, "", false
	}
	if !until.After(now) {
		until = time.Time{}
	}
	return Signal{Vendor: adapter.Codex, Until: until, Via: ViaRollout}, at, true
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
