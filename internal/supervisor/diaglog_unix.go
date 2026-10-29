//go:build unix

package supervisor

import (
	"errors"
	"sort"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// codedError is a refusal with its reason code (docs/logging.md); its
// text is the error's own.
type codedError struct {
	code diag.Code
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// coded gives err the reason code c, unless it has one already.
func coded(c diag.Code, err error) error {
	var ce *codedError
	if err == nil || errors.As(err, &ce) {
		return err
	}
	return &codedError{c, err}
}

// codeOf is err's reason code, or def.
func codeOf(err error, def diag.Code) diag.Code {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return def
}

// log queues r about the session: its parent, the on-screen child, and
// the vendor on screen unless r names another. It never waits.
func (s *session) log(r diag.Rec) string {
	r.Parent, r.Child = s.o.ParentID, s.o.ChildID
	if r.Vendor == "" {
		r.Vendor = string(s.o.Vendor)
	}
	return s.o.Diag.Log(r)
}

// logWait is log for the socket's goroutines, which are not the
// terminal loop: it waits up to sockLogWait for room in a full queue.
func (s *session) logWait(r diag.Rec) {
	r.Parent, r.Child = s.o.ParentID, s.o.ChildID
	if r.Vendor == "" {
		r.Vendor = string(s.o.Vendor)
	}
	s.o.Diag.LogWait(r, sockLogWait)
}

// sockLogWait bounds how long a socket goroutine waits for room in the
// diagnostic queue; a hook waits for the answer, up to 3 s.
const sockLogWait = 100 * time.Millisecond

// hookName is a hook event's name for a record: the names aggy acts on,
// else "other", since any process in the CLI's tree can pick the name.
func hookName(event string) string {
	switch event {
	case "Stop", "StopFailure", "StopCancelled", "SessionStart", "Notification":
		return event
	}
	return "other"
}

// foreignSeen is the turn ends of one foreign session, or of all those
// past maxForeign: when the last record was written, how many came
// since, and the latest one's event.
type foreignSeen struct {
	at      time.Time
	repeats int
	event   string
}

// maxForeign caps the foreign sessions kept apart; turn ends of more,
// while every kept one is within its minute, share foreignOver.
const maxForeign = 64

// ignoredTurnEnd records a turn end that named another session, at most
// once a minute per session, with how many came since the last record.
// A turn end naming no session is keyed by "" (kept apart even past the
// cap) and recorded the same way, as a security rejection (G67-R1).
// Memory and records stay bounded however many ids are cycled through
// (G67-R3): a session whose minute has passed is let go, its repeats
// recorded first, and the rest past the cap are counted together.
func (s *session) ignoredTurnEnd(ev hookEvent) {
	now := time.Now()
	if s.foreign == nil {
		s.foreign = map[string]*foreignSeen{}
	}
	id := ev.sessionID
	f, over := s.foreign[id], false
	if f == nil && id != "" && len(s.foreign) >= maxForeign {
		s.flushForeign(func(f *foreignSeen) bool { return now.Sub(f.at) >= time.Minute })
	}
	if f == nil {
		if id != "" && len(s.foreign) >= maxForeign {
			f, over = &s.foreignOver, true
		} else {
			f = &foreignSeen{}
			s.foreign[id] = f
		}
	}
	if !f.at.IsZero() && now.Sub(f.at) < time.Minute {
		f.repeats++
		f.event = ev.name
		return
	}
	attrs := map[string]any{"repeats": f.repeats}
	if over {
		attrs["over_cap"] = true
	}
	*f = foreignSeen{at: now, event: ev.name}
	s.logIgnored(id == "", ev.name, attrs)
}

// flushForeign records the repeats not yet recorded of each kept session
// that drop selects, and lets it go; with drop nil, of every one and of
// foreignOver too, for the end of the session (G67-R3). Such a record
// (attrs.tail) stands for its repeats alone, not a turn end of its own.
// Sessions go in id order, so the records do not follow map order.
func (s *session) flushForeign(drop func(*foreignSeen) bool) {
	ids := make([]string, 0, len(s.foreign))
	for id := range s.foreign {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := s.foreign[id]
		if drop != nil && !drop(f) {
			continue
		}
		if f.repeats > 0 {
			s.logIgnored(id == "", f.event, map[string]any{"repeats": f.repeats, "tail": true})
		}
		delete(s.foreign, id)
	}
	if drop == nil && s.foreignOver.repeats > 0 {
		s.logIgnored(false, s.foreignOver.event, map[string]any{"repeats": s.foreignOver.repeats, "tail": true, "over_cap": true})
		s.foreignOver = foreignSeen{}
	}
}

// logIgnored writes the record of an ignored turn end, with attrs and
// its event: a security rejection when it named no session though the
// vendor's hook always names one, else an ignored foreign turn end.
func (s *session) logIgnored(noSession bool, event string, attrs map[string]any) {
	attrs["event"] = hookName(event)
	if noSession && turnEndNamesSession(s.o.Vendor, event) {
		// A tail only counts repeats of a refusal aggy log already has.
		s.log(diag.Rec{Event: diag.EvHookRejected, Code: diag.CodeNoSession, Msg: "a turn end that names no session", Attrs: attrs, NoRefusal: attrs["tail"] == true})
		return
	}
	if noSession {
		attrs["no_session"] = true
	}
	s.log(diag.Rec{Event: diag.EvHookIgnored, Code: diag.CodeForeignSession, Msg: "a turn end of another session", Attrs: attrs})
}

// turnEndNamesSession reports whether v's own hook always names its
// session in a turn end event, so one naming none looks forged
// (hook.no_session, a security rejection). It is the one place that
// decides it.
func turnEndNamesSession(v adapter.Vendor, event string) bool {
	if v == adapter.Grok && (event == "StopFailure" || event == "StopCancelled") {
		return grokStopNamesSession
	}
	return true
}

// grokStopNamesSession: whether Grok's StopFailure and StopCancelled
// payloads always carry sessionId.
//
// TODO(T6807-R2): unverified; a live check decides. If they can come
// without one, set this false, so such a turn end is an ignored one
// (no_session) rather than a security rejection, and key those apart
// from the "" entry in ignoredTurnEnd.
const grokStopNamesSession = true

// switchAttrs are a switch's options as record attrs: never the model's
// name, only whether one was set.
func switchAttrs(c Choice, more map[string]any) map[string]any {
	m := map[string]any{"brief": c.Brief, "new": c.New, "model_set": c.Model != "", "child_set": c.Child != "", "tools": len(c.Tools)}
	for k, v := range more {
		m[k] = v
	}
	return m
}
