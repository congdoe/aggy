//go:build unix

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
)

// diagHome gives the test a state dir of its own and a queue whose
// records go to its diag.db; records returns them once the queue is
// closed.
func diagHome(t *testing.T) (*diag.Queue, func() []store.DiagRecord) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", state)
	t.Setenv(diag.TraceEnv, "")
	q := diag.New("v-test", io.Discard).Queue(256)
	return q, func() []store.DiagRecord {
		t.Helper()
		// Close returns once every queued record is written; the long
		// limit only stops a hung writer, so a loaded machine never reads
		// a partial log (F73-1).
		q.Close(5 * time.Minute)
		v, err := store.OpenDiagView(context.Background(), filepath.Join(state, store.DiagFileName), "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = v.Close() }()
		recs, err := v.Records(context.Background(), store.DiagFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return recs
	}
}

// find returns the first record of ev (and code, when not zero).
func find(recs []store.DiagRecord, ev diag.Event, code diag.Code) (store.DiagRecord, bool) {
	for _, r := range recs {
		if r.Event == ev.String() && (code.IsZero() || r.Code == code.String()) {
			return r, true
		}
	}
	return store.DiagRecord{}, false
}

// TestDiagSwitchRecords (T6753): a switch the user picks is recorded from
// the pick to the CLI's exit, each step naming the one before as its
// cause; a nested CLI's turn end is a foreign-session rejection; and none
// of what the user typed, a hook payload held, or the CLI printed is
// stored.
func TestDiagSwitchRecords(t *testing.T) {
	const typed, said, printed = "typed-ocelot-canary-41", "payload-marmot-canary-42", "stderr-pelican-canary-43"
	q, records := diagHome(t)
	ts, c, done, sock := switchRunOn(t, true, adapter.Claude, func(o *Options) { o.Diag, o.ChildID = q, "child-1" })
	c.print("error: " + printed + "\n")
	ts.typeIn(t, "\x1c2")
	poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
	// Unsent text holds the switch at the turn end.
	ts.typeIn(t, typed)
	c.waitGot(t, []byte(typed))
	_ = request(t, sock, `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"session_id":"`+childSID+`","error":"`+said+`"}}`)
	poll(t, "the unsent-text note", []byte("you have not sent"), ts.screen)
	mark := len(ts.screen())
	ts.typeIn(t, "\x1b")
	poll(t, "the note to close", []byte(altScreenOff), func() []byte { return ts.screen()[mark:] })
	// Cleared and sent empty: the next turn end offers it. Run follows the
	// composer before it passes keys on, so once the CLI has them the
	// turn ends below cannot overtake them.
	ts.typeIn(t, "\x15\r")
	c.waitGot(t, []byte(typed+"\x15\r"))
	_ = request(t, sock, `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"session_id":"`+said+`"}}`)
	_ = request(t, sock, `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"session_id":"`+childSID+`"}}`)
	poll(t, "the confirmation", []byte("-> grok. Ctrl+"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})

	recs := records()
	for _, r := range recs {
		all := r.ID + r.Msg + string(r.Attrs) + r.Code + r.Event + r.VendorSessionID
		for _, leak := range []string{typed, said, printed, childSID} {
			if strings.Contains(all, leak) {
				t.Errorf("record %s holds %q: %+v", r.Event, leak, r)
			}
		}
		if r.ParentID != pid || r.ChildID != "child-1" || r.TraceID == "" {
			t.Errorf("record %s: parent %q child %q trace %q", r.Event, r.ParentID, r.ChildID, r.TraceID)
		}
	}
	for _, w := range []struct {
		ev   diag.Event
		code diag.Code
	}{
		{diag.EvPickerOpened, diag.Code{}}, {diag.EvPickerChosen, diag.Code{}},
		{diag.EvSwitchRefused, diag.CodeUnsentText}, {diag.EvHookIgnored, diag.CodeForeignSession},
	} {
		if _, ok := find(recs, w.ev, w.code); !ok {
			t.Errorf("no %s %s record", w.ev, w.code)
		}
	}
	// The chain: queued <- offered (by the turn end) <- confirmed <- completed.
	queued, _ := find(recs, diag.EvSwitchQueued, diag.Code{})
	offered, _ := find(recs, diag.EvSwitchOffered, diag.Code{})
	confirmed, _ := find(recs, diag.EvSwitchConfirmed, diag.Code{})
	completed, ok := find(recs, diag.EvSwitchCompleted, diag.Code{})
	if !ok || queued.ID == "" || completed.CauseID != confirmed.ID || confirmed.CauseID != offered.ID || offered.CauseID == "" {
		t.Errorf("switch chain: queued %s, offered %s<-%s, confirmed %s<-%s, completed <-%s",
			queued.ID, offered.ID, offered.CauseID, confirmed.ID, confirmed.CauseID, completed.CauseID)
	}
	var stop store.DiagRecord
	for _, r := range recs {
		if r.ID == offered.CauseID {
			stop = r
		}
	}
	if stop.Event != diag.EvHookAccepted.String() || !strings.Contains(string(stop.Attrs), `"counted":true`) {
		t.Errorf("the offer's cause is %+v, want the turn end it followed", stop)
	}
}

// TestDiagOfferCause (G67-2): a nested CLI's Stop right after the
// on-screen one, before the offer is polled, does not become the offer's
// cause; the counted turn end does. Foreign turn ends are recorded once
// a minute per session, as ignored, not as refusals; one naming no
// session is a security rejection.
func TestDiagOfferCause(t *testing.T) {
	q, records := diagHome(t)
	ts, c, done, sock := switchRunOn(t, true, adapter.Claude, func(o *Options) { o.Diag = q })
	ts.typeIn(t, "\x1c2")
	poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
	_ = request(t, sock, stopLine)
	for i := 0; i < 3; i++ {
		_ = request(t, sock, nestedStop)
	}
	for i := 0; i < 3; i++ {
		_ = request(t, sock, idlessStop)
	}
	poll(t, "the confirmation", []byte("-> grok. Ctrl+"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(100 * time.Millisecond)
	finish(t, c, done)
	recs := records()
	offered, ok := find(recs, diag.EvSwitchOffered, diag.Code{})
	if !ok {
		t.Fatal("no offer record")
	}
	for _, r := range recs {
		if r.ID == offered.CauseID && (r.Event != diag.EvHookAccepted.String() || !strings.Contains(string(r.Attrs), `"counted":true`)) {
			t.Errorf("the offer's cause is %s %s, want the counted turn end", r.Event, r.Attrs)
		}
		if r.Event == diag.EvHookIgnored.String() && r.Code != diag.CodeForeignSession.String() {
			t.Errorf("ignored with code %q", r.Code)
		}
		if r.Code == diag.CodeForeignSession.String() && r.Event != diag.EvHookIgnored.String() {
			t.Errorf("foreign session recorded as %s", r.Event)
		}
	}
	// Three foreign turn ends in a minute are one record, and the two
	// repeats are recorded when the session ends (G67-R3). G67-R1: the
	// same for three naming no session, as security records.
	if n, sum := countTurnEnds(t, recs, diag.CodeForeignSession); n != 2 || sum != 3 {
		t.Errorf("%d ignored records for %d turn ends, want 2 for 3", n, sum)
	}
	if n, sum := countTurnEnds(t, recs, diag.CodeNoSession); n != 2 || sum != 3 {
		t.Errorf("%d hook.no_session records for %d turn ends, want 2 for 3", n, sum)
	}
}

// countTurnEnds is how many records have code c, and how many turn ends
// they stand for: its attrs.repeats, and itself unless it is a tail.
func countTurnEnds(t *testing.T, recs []store.DiagRecord, c diag.Code) (n, sum int) {
	t.Helper()
	for _, r := range recs {
		if r.Code != c.String() {
			continue
		}
		var a struct {
			Repeats int
			Tail    bool
		}
		if err := json.Unmarshal(r.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		n++
		sum += a.Repeats
		if !a.Tail {
			sum++
		}
	}
	return n, sum
}

// TestIgnoredTurnEndBounded (G67-R3): cycling through more foreign
// session ids than maxForeign neither loses a turn end nor records each
// one: the sessions past the cap are counted together, a session whose
// minute has passed is let go with its repeats recorded, and the end of
// the session records what is left.
func TestIgnoredTurnEndBounded(t *testing.T) {
	q, records := diagHome(t)
	s := &session{o: Options{Diag: q, Vendor: adapter.Claude}}
	const ids, rounds = maxForeign + 36, 3
	for r := 0; r < rounds; r++ {
		for i := 0; i < ids; i++ {
			s.ignoredTurnEnd(hookEvent{name: "Stop", sessionID: fmt.Sprintf("sess-%03d", i)})
		}
	}
	if len(s.foreign) != maxForeign {
		t.Errorf("%d sessions kept, want %d", len(s.foreign), maxForeign)
	}
	// A minute later, a new id lets the old sessions go, recording their
	// repeats, and is kept on its own.
	for _, f := range s.foreign {
		f.at = f.at.Add(-2 * time.Minute)
	}
	s.ignoredTurnEnd(hookEvent{name: "Stop", sessionID: "sess-new"})
	if len(s.foreign) != 1 || s.foreign["sess-new"] == nil {
		t.Errorf("kept %d sessions after expiry, want sess-new alone", len(s.foreign))
	}
	s.ignoredTurnEnd(hookEvent{name: "Stop", sessionID: "sess-new"})
	s.flushForeign(nil)
	recs := records()
	n, sum := countTurnEnds(t, recs, diag.CodeForeignSession)
	if sum != ids*rounds+2 {
		t.Errorf("records stand for %d turn ends, want %d", sum, ids*rounds+2)
	}
	// Per kept session a first record and its repeats, the same for the
	// sessions past the cap, and sess-new's two.
	if want := 2*maxForeign + 2 + 2; n != want {
		t.Errorf("%d records, want %d", n, want)
	}
	var over int
	for _, r := range recs {
		if strings.Contains(string(r.Attrs), `"over_cap":true`) {
			over++
		}
	}
	if over != 2 {
		t.Errorf("%d over-cap records, want 2", over)
	}
}

// TestTurnEndNamesSession (T6807-R2): the one switch for Grok's stop
// events, and every other vendor's turn end must name its session.
func TestTurnEndNamesSession(t *testing.T) {
	for _, c := range []struct {
		v     adapter.Vendor
		event string
		want  bool
	}{
		{adapter.Claude, "Stop", true}, {adapter.Claude, "StopFailure", true}, {adapter.Codex, "Stop", true},
		{adapter.Grok, "Stop", true}, {adapter.Grok, "StopFailure", grokStopNamesSession}, {adapter.Grok, "StopCancelled", grokStopNamesSession},
	} {
		if got := turnEndNamesSession(c.v, c.event); got != c.want {
			t.Errorf("turnEndNamesSession(%s, %s) = %v", c.v, c.event, got)
		}
	}
}

// TestDiagAgentRefusals (T6753): an agent's switch-request refused for
// each reason answers with its code and records it; Codex has no hold
// signal.
func TestDiagAgentRefusals(t *testing.T) {
	q, records := diagHome(t)
	_, c, done, sock := switchRunOn(t, true, adapter.Codex, func(o *Options) { o.Diag = q })
	r := request(t, sock, `{"type":"switch-request","vendor":"grok"}`)
	if !strings.Contains(r, `"code":"switch.agent_no_hold"`) {
		t.Errorf("reply %q", r)
	}
	finish(t, c, done)
	if _, ok := find(records(), diag.EvSwitchRefused, diag.CodeAgentNoHold); !ok {
		t.Error("no switch.agent_no_hold record")
	}
}

// TestAnswerRecords: what the socket refuses or does not act on is
// recorded with its code, and the reply carries the code.
func TestAnswerRecords(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		from     bool
		onSwitch func(Request) error
		want     diag.Code
		reply    string
	}{
		{"bad JSON", `{"type":"hook-event"`, true, nil, diag.CodeBadRequest, diag.CodeBadRequest.String()},
		{"a hook from outside", stopLine, false, nil, diag.CodeLineage, ""},
		{"a switch from outside", `{"type":"switch-request","vendor":"grok"}`, false, nil, diag.CodeLineage, diag.CodeLineage.String()},
		{"no pass-through", `{"type":"switch-request","vendor":"grok"}`, true, nil, diag.CodeNotSupervised, diag.CodeNotSupervised.String()},
		{"agent options", `{"type":"switch-request","vendor":"grok","new":true}`, true,
			func(Request) error { return coded(diag.CodeAgentOptions, errors.New("no")) }, diag.CodeAgentOptions, diag.CodeAgentOptions.String()},
		{"plain error", `{"type":"switch-request","vendor":"grok"}`, true,
			func(Request) error { return errors.New("not offered") }, diag.CodeSwitchVendor, diag.CodeSwitchVendor.String()},
		{"from the session", stopLine, true, nil, diag.Code{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []diag.Rec
			resp := answer([]byte(tt.line), tt.from, nil, tt.onSwitch, nil, func(r diag.Rec) { got = append(got, r) })
			if resp.Code != tt.reply {
				t.Errorf("reply code %q, want %q", resp.Code, tt.reply)
			}
			if tt.want.IsZero() {
				if len(got) != 0 {
					t.Errorf("records %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Code != tt.want {
				t.Errorf("records %+v, want one %s", got, tt.want)
			}
		})
	}
}

// TestGrokShutdownStop (T6847): Grok's exit-time stop ("reason":
// "shutdown", live 2026-10-29, grok 1.0.70) ends no turn: no atPrompt,
// no warnDue, no share prompt, and the preselect stays. It is recorded
// once a minute as an ignored hook event. Another reason, another event,
// or another vendor is unchanged.
func TestGrokShutdownStop(t *testing.T) {
	q, records := diagHome(t)
	newSession := func(v adapter.Vendor) *session {
		return &session{o: Options{Diag: q, Vendor: v, SessionID: func() string { return "me" }},
			events: make(chan hookEvent, 1), quit: make(chan struct{}), pk: &picker{preselect: adapter.Codex}}
	}
	deliver := func(s *session, event, payload string) {
		s.onHook(Request{Type: ReqHookEvent, Vendor: string(s.o.Vendor), Event: event, FromSession: true, Payload: []byte(payload)})
		s.hookEvent(<-s.events)
	}
	s := newSession(adapter.Grok)
	for range 3 {
		deliver(s, "Stop", `{"sessionId":"me","reason":"shutdown"}`)
	}
	if s.atPrompt || s.warnDue || s.pk.preselect != adapter.Codex || s.lastTurnEnd != "" {
		t.Errorf("a shutdown stop counted as a turn end: atPrompt %v warnDue %v preselect %q", s.atPrompt, s.warnDue, s.pk.preselect)
	}
	if s.shutdownSeen.repeats != 2 {
		t.Errorf("repeats %d, want 2", s.shutdownSeen.repeats)
	}
	for _, c := range []struct {
		v              adapter.Vendor
		event, payload string
	}{
		{adapter.Grok, "Stop", `{"sessionId":"me","reason":"Shutdown"}`},
		{adapter.Grok, "Stop", `{"sessionId":"me"}`},
		{adapter.Grok, "StopCancelled", `{"sessionId":"me","reason":"shutdown"}`},
		{adapter.Claude, "Stop", `{"session_id":"me","reason":"shutdown"}`},
		{adapter.Codex, "Stop", `{"session_id":"me","reason":"shutdown"}`},
	} {
		s := newSession(c.v)
		deliver(s, c.event, c.payload)
		if !s.atPrompt || !s.warnDue {
			t.Errorf("%s %s %s: not a turn end", c.v, c.event, c.payload)
		}
	}
	recs := records()
	n := 0
	for _, r := range recs {
		if r.Event == diag.EvHookIgnored.String() && r.Code == diag.CodeGrokShutdown.String() {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d hook.grok_shutdown records, want 1", n)
	}
}
