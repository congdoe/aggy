//go:build unix

package supervisor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/hook"
)

// turned is a tracker on v with one counted turn end, so it has a baseline.
func turned(v adapter.Vendor) *session {
	s := &session{toChild: newByteQueue(maxQueued), lenKnown: true, atPrompt: true, quit: make(chan struct{}),
		o: Options{Vendor: v, SessionID: func() string { return childSID }}}
	s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
	return s
}

// TestTabKeepsDraft (T6897): Tab never empties the composer in aggy's count: on
// Codex a ! command and a $ popup keep the text, and other keys only look like Tab.
func TestTabKeepsDraft(t *testing.T) {
	for _, tt := range []struct {
		name, draft, tab string
		queues           int
	}{
		{"a ! command", "!ls", "\t", 1},
		{"a $ popup", "$he", "\t", 1},
		{"plain text", "more", "\t", 1},
		{"kitty tab", "more", "\x1b[9u", 1},
		{"kitty tab with Caps Lock", "more", "\x1b[9;65u", 1},
		{"kitty repeat", "more", "\x1b[9;1:2u", 1},
		{"kitty shift+tab", "more", "\x1b[9;2u", 0},
		{"kitty ctrl+tab", "more", "\x1b[9;5u", 0},
		{"kitty release", "more", "\x1b[9;1:3u", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := turned(adapter.Codex)
			s.atPrompt = false // a turn runs
			s.send([]byte(tt.draft))
			s.send([]byte(tt.tab))
			if !s.dirty || s.compLen == 0 {
				t.Errorf("dirty %v, compLen %d", s.dirty, s.compLen)
			}
			if s.queued != tt.queues {
				t.Errorf("queued = %d, want %d", s.queued, tt.queues)
			}
		})
	}
}

// waitDirtyNote picks the vendor twice, which offers unless text blocks it, and
// expects the unsent-text note with no exit command typed.
func waitDirtyNote(t *testing.T, ts *termSide, c *fakeChild) {
	t.Helper()
	ts.typeIn(t, "\x1c1")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "\x1c")
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "1")
	poll(t, "the unsent-text note", []byte("you typed text you have not sent"), ts.screen)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Errorf("the child received %q", c.received())
	}
}

// TestExitNotTypedOnDraft (T6897): through Run, the two ways a draft used to be
// lost under /exit: a Tab that did not send it, and a backtrack that restored it.
func TestExitNotTypedOnDraft(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []string
	}{
		{"a ! command and Tab", []string{"!ls", "\t"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, _ := switchRunTurned(t, adapter.Codex)
			for _, k := range tt.keys {
				ts.typeIn(t, k)
				c.waitGot(t, []byte(k))
				time.Sleep(70 * time.Millisecond) // past splitWait, as a person types
			}
			waitDirtyNote(t, ts, c)
			finish(t, c, done)
		})
	}
}

// TestBacktrackAsksFirst (T6897): Esc, Esc, Enter may restore a prompt on Codex,
// with h, k, or Ctrl+T in between going to its preview. aggy cannot see that, so
// it says so at the first pick, and at the second the status line says so too;
// nothing is typed until the person confirms.
func TestBacktrackAsksFirst(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []string
	}{
		{"Esc, Esc, Enter", []string{"\x1b", "\x1b", "\r"}},
		{"Esc, Esc, h, Enter", []string{"\x1b", "\x1b", "h", "\r"}},
		{"Esc, Esc, k, Enter", []string{"\x1b", "\x1b", "k", "\r"}},
		{"Esc, Esc, Ctrl+T, Enter", []string{"\x1b", "\x1b", "\x14", "\r"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, _ := switchRunTurned(t, adapter.Codex, func(*Options) { waitNoteQuiet = 30 * time.Millisecond })
			for _, k := range tt.keys {
				ts.typeIn(t, k)
				c.waitGot(t, []byte(k))
				time.Sleep(70 * time.Millisecond)
			}
			// The Enter may have started a turn, so the note says that too, and
			// the next pick both says none runs and confirms.
			ts.typeIn(t, "\x1c1")
			poll(t, "the note", []byte("cannot tell if the prompt is empty"), ts.screen)
			ts.typeIn(t, "\x1b")
			time.Sleep(escTimeout + 60*time.Millisecond)
			if bytes.Contains(c.received(), []byte("/exit")) {
				t.Fatalf("typed before any confirm: %q", c.received())
			}
			seen := len(ts.screen())
			ts.typeIn(t, "\x1c")
			time.Sleep(60 * time.Millisecond)
			ts.typeIn(t, "1")
			since := func() []byte { return ts.screen()[seen:] }
			poll(t, "the status line", []byte("can't tell if the prompt is empty"), since)
			if bytes.Contains(c.received(), []byte("/exit")) {
				t.Fatalf("typed before the confirm: %q", c.received())
			}
			finish(t, c, done)
		})
	}
}

// TestEscThenTextOnce: a single Esc, then a message, is the everyday interrupt
// and stays clean; two Escs do not, until a turn from it ends.
func TestEscThenText(t *testing.T) {
	s := turned(adapter.Codex)
	for _, k := range []string{"\x1b", "hello", "\r"} {
		s.send([]byte(k))
	}
	if s.dirty || s.doubt() {
		t.Errorf("one Esc: dirty %v, doubt %v", s.dirty, s.doubt())
	}
	s = turned(adapter.Codex)
	for _, k := range []string{"\x1b", "\x1b", "hello", "\r"} {
		s.send([]byte(k))
	}
	if !s.doubt() {
		t.Error("two Escs: no doubt")
	}
	s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
	if s.doubt() || s.dirty {
		t.Errorf("after the turn: dirty %v, doubt %v", s.dirty, s.doubt())
	}
}

// TestTurnEndEvidence (T6897): when a counted turn end clears doubt.
func TestTurnEndEvidence(t *testing.T) {
	stop := hookEvent{name: "Stop", sessionID: childSID, chain: ownChain}
	foreign := hookEvent{name: "Stop", sessionID: nestedSID, chain: nestedChain}
	for _, tt := range []struct {
		name  string
		keys  []string
		ends  []hookEvent
		doubt bool
		known bool
	}{
		{"@file in a message: a turn ran from the Enter", []string{"fix @a.go please\r"}, []hookEvent{stop}, false, false},
		{"a command that ran a turn", []string{"/compact\r"}, []hookEvent{stop}, false, false},
		{"@ and Enter with no turn end", []string{"@fi\r"}, nil, true, false},
		{"a foreign turn end is no evidence", []string{"fix @a.go\r"}, []hookEvent{foreign}, true, false},
		{"input since the Enter", []string{"@fi\r", "x"}, []hookEvent{stop}, true, false},
		{"a key since the Enter", []string{"@fi\r", "\x1b[A"}, []hookEvent{stop}, true, false},
		{"text typed after a clean turn stays known", []string{"go\r", "draft"}, []hookEvent{stop}, false, true},
		{"an @ Enter during a turn: no evidence", []string{"go\r", "@fi\r"}, []hookEvent{stop}, true, false},
		{"Esc mid-turn, then an approval's Enter", []string{"go\r", "\x1b", "\r"}, []hookEvent{stop}, false, false},
		{"Esc mid-turn, Up, Enter", []string{"go\r", "\x1b", "\x1b[A", "\r"}, []hookEvent{stop}, false, false},
		{"Esc with no Enter since stays", []string{"go\r", "\x1b"}, []hookEvent{stop}, true, false},
		// What can go wrong: a turn a background task starts ends just after a
		// popup took the Enter. This documents the limit (docs/design.md).
		{"limit: a background turn after a popup's Enter", []string{"@fi\r"}, []hookEvent{stop}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := turned(adapter.Claude)
			for _, k := range tt.keys {
				s.send([]byte(k))
			}
			for _, e := range tt.ends {
				s.hookEvent(e)
			}
			if s.doubt() != tt.doubt || s.knownText() != tt.known {
				t.Errorf("doubt %v, known %v; want %v, %v (compLen %d, lenKnown %v, popup %v, esc %v %v)",
					s.doubt(), s.knownText(), tt.doubt, tt.known, s.compLen, s.lenKnown, s.popup, s.escEmpty, s.escDoubt)
			}
		})
	}
}

// TestDoubtLinesFit: the note's lines fit 80 columns.
func TestDoubtLinesFit(t *testing.T) {
	for _, on := range adapter.Vendors() {
		for _, to := range adapter.Vendors() {
			for _, l := range doubtLines("switch to "+string(to), on, to) {
				if len(l) > 80 {
					t.Errorf("%d columns: %q", len(l), l)
				}
			}
		}
	}
}

// TestKeyAfterConfirmAsksAgain: the confirm is for the prompt the person saw. A
// key after it needs a new look, and text is known again.
func TestKeyAfterConfirmAsksAgain(t *testing.T) {
	s := turned(adapter.Claude)
	s.send([]byte("@fi\r"))
	s.sw.toldDoubt, s.sw.doubtOK = true, true
	s.send([]byte("\x1b[A"))
	if s.sw.toldDoubt || s.sw.doubtOK {
		t.Errorf("told %v, ok %v", s.sw.toldDoubt, s.sw.doubtOK)
	}
}

// realQueuedQuiet is the value a user gets: the harness shortens queuedQuiet.
var realQueuedQuiet = queuedQuiet

// TestDailyClaude (T6897): one ordinary Claude session per row, and what picking
// grok brings. An ordinary session must not be refused for text that is not there,
// and no pick may wait with nothing said. "first" and "second" are what shows after
// the first pick, and after Esc and a second one; "" means none is needed. Rows
// marked real keep the real queuedQuiet, which the harness otherwise shortens and
// which would hide a wait. This pins the whole table, so a change to it is a
// decision.
func TestDailyClaude(t *testing.T) {
	const (
		line  = "status line"
		doubt = "note: cannot tell"
		moved = "note: moved"
		waits = "note: waits"
		both  = "note: waits, cannot tell"
	)
	type step struct{ kind, keys string }
	k := func(s string) step { return step{"k", s} }
	S, M, Sm, C := step{kind: "S"}, step{kind: "M"}, step{kind: "m"}, step{kind: "C"}
	for _, tt := range []struct {
		name          string
		steps         []step
		real          bool
		first, second string
	}{
		{"message, turn end", []step{k("hello\r"), S}, false, line, ""},
		{"/clear, no message", []step{k("/clear\r"), M}, false, line, ""},
		{"/clear, message, turn end", []step{k("/clear\r"), M, k("hello\r"), Sm}, false, line, ""},
		{"/clear, message, turn end, real timing", []step{k("/clear\r"), M, k("hello\r"), Sm}, true, line, ""},
		{"/clear with a name, no hook", []step{k("/clear old\r")}, false, both, line},
		{"/compact, compact hook, no turn end", []step{k("/compact\r"), C}, false, both, line},
		{"/compact, its turn end", []step{k("/compact\r"), C, S}, false, line, ""},
		{"/compact, a message, turn end", []step{k("/compact\r"), C, k("hello\r"), S}, false, doubt, line},
		{"/compact, a message, turn end, real timing", []step{k("/compact\r"), C, k("hello\r"), S}, true, both, line},
		{"/model closed with Esc, a message", []step{k("/model\r"), k("\x1b"), k("hello\r"), S}, false, line, ""},
		{"/model, real timing", []step{k("/model\r"), k("\x1b"), k("hello\r"), S}, true, line, ""},
		{"/model still open", []step{k("/model\r")}, false, doubt, line},
		{"/cost, 2 messages", []step{k("/cost\r"), k("one\r"), S, k("two\r"), S}, false, line, ""},
		{"/cost, 2 messages, real timing", []step{k("/cost\r"), k("one\r"), S, k("two\r"), S}, true, line, ""},
		{"/status, nothing else", []step{k("/status\r")}, false, doubt, line},
		{"a custom command, no turn end", []step{k("/mine\r")}, false, both, line},
		{"@file in a message", []step{k("fix @a.go please\r"), S}, false, line, ""},
		{"@file, no turn end", []step{k("@fi\r")}, false, both, line},
		{"a message, no turn end yet", []step{k("go\r")}, false, waits, line},
		{"Esc interrupts, a message", []step{k("go\r"), k("\x1b"), k("hello\r"), S}, false, line, ""},
		{"Esc interrupts, a message, real timing", []step{k("go\r"), k("\x1b"), k("hello\r"), S}, true, waits, line},
		{"a message sent during a turn, real timing", []step{k("go\r"), k("more\r"), S}, true, waits, line},
		{"Esc on an empty prompt", []step{k("\x1b")}, false, doubt, line},
		{"Esc interrupts, Up, Enter", []step{k("go\r"), k("\x1b"), k("\x1b[A"), k("\r"), S}, false, line, ""},
		{"approval with Enter", []step{k("go\r"), k("\r"), S}, false, line, ""},
		{"approval: Down, Enter", []step{k("go\r"), k("\x1b[B"), k("\r"), S}, false, line, ""},
		{"approval: Down, Enter, real timing", []step{k("go\r"), k("\x1b[B"), k("\r"), S}, true, line, ""},
		{"Esc mid-turn, approval with Enter", []step{k("go\r"), k("\x1b"), k("\r"), S}, false, line, ""},
		{"a click", []step{k("hello\r"), S, k("\x1b[<0;10;5M"), k("\x1b[<0;10;5m")}, false, line, ""},
		{"focus out and in", []step{k("hello\r"), S, k("\x1b[O"), k("\x1b[I")}, false, line, ""},
		{"wheel", []step{k("hello\r"), S, k("\x1b[<64;10;5M")}, false, line, ""},
		{"text, Ctrl+U, Enter", []step{k("abc"), k("\x15"), k("\r")}, false, waits, line},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, sock := switchRunTurned(t, adapter.Claude, func(*Options) {
				// The note comes after a shortened release, and well before a real one.
				waitNoteQuiet, queuedQuiet = 150*time.Millisecond, 50*time.Millisecond
				if tt.real {
					queuedQuiet = realQueuedQuiet
				}
			})
			for _, st := range tt.steps {
				switch st.kind {
				case "k":
					ts.typeIn(t, st.keys)
					c.waitGot(t, []byte(st.keys))
				case "S":
					_ = request(t, sock, stopLine)
				case "m":
					_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"session_id":%q}}`, movedSID))
				case "M":
					_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"SessionStart","payload":{"session_id":%q,"source":"clear"}}`, movedSID))
				case "C":
					_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"SessionStart","payload":{"session_id":%q,"source":"compact"}}`, childSID))
				}
				time.Sleep(70 * time.Millisecond)
			}
			shown := func(seen int) string {
				out := ts.screen()[seen:]
				has := func(w string) bool { return bytes.Contains(out, []byte(w)) }
				switch {
				case has("Esc stays"):
					return line
				case has("have not sent"):
					return "note: unsent"
				case has("started a turn") && has("prompt is empty"):
					return both
				case has("started a turn"):
					return waits
				case has("prompt is empty"):
					return doubt
				case has("moved to another session"):
					return moved
				case has("waiting for you"):
					return "note: held"
				}
				return "SILENT"
			}
			seen := len(ts.screen())
			ts.typeIn(t, "\x1c2")
			time.Sleep(400 * time.Millisecond)
			got := shown(seen)
			t.Logf("DAILY | %-44s | first: %s", tt.name, got)
			if got != tt.first {
				t.Errorf("first pick: %q, want %q", got, tt.first)
			}
			if tt.second != "" {
				ts.typeIn(t, "\x1b")
				time.Sleep(escTimeout + 60*time.Millisecond)
				seen = len(ts.screen())
				ts.typeIn(t, "\x1c")
				time.Sleep(60 * time.Millisecond)
				ts.typeIn(t, "2")
				time.Sleep(400 * time.Millisecond)
				got = shown(seen)
				t.Logf("DAILY | %-44s | second: %s", tt.name, got)
				if got != tt.second {
					t.Errorf("second pick: %q, want %q", got, tt.second)
				}
			}
			finish(t, c, done)
		})
	}
}

// TestCompactThirdPick (T6897): /compact with no turn end reaches the
// status line at the second pick. The person's Esc there drops the switch, and a
// third pick starts over, with the prompt as it was: the note, not a silent wait.
func TestCompactThirdPick(t *testing.T) {
	ts, c, done, sock := switchRunTurned(t, adapter.Claude, func(*Options) { waitNoteQuiet = 50 * time.Millisecond })
	ts.typeIn(t, "/compact\r")
	c.waitGot(t, []byte("/compact\r"))
	_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"SessionStart","payload":{"session_id":%q,"source":"compact"}}`, childSID))
	time.Sleep(70 * time.Millisecond)
	ts.typeIn(t, "\x1c2")
	poll(t, "the first note", []byte("started a turn"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(escTimeout + 60*time.Millisecond)
	seen := len(ts.screen())
	since := func() []byte { return ts.screen()[seen:] }
	ts.typeIn(t, "\x1c")
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "2")
	poll(t, "the status line", []byte("can't tell if the prompt is empty"), since)
	ts.typeIn(t, "\x1b")
	time.Sleep(escTimeout + 60*time.Millisecond)
	seen = len(ts.screen())
	ts.typeIn(t, "\x1c")
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "2")
	poll(t, "the note again", []byte("prompt is empty"), since)
	if bytes.Contains(since(), []byte("Esc stays")) {
		t.Errorf("a new pick offered with no look: %q", since())
	}
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Errorf("typed: %q", c.received())
	}
	finish(t, c, done)
}

// TestLocalCommandKeepsPrompt (T6897): exact, typed lines only. A cursor key, an
// edit, arguments, or another vendor is no local command; what follows is then an
// ordinary Enter at the prompt or a queued message.
func TestLocalCommandKeepsPrompt(t *testing.T) {
	for _, tt := range []struct {
		name string
		v    adapter.Vendor
		keys []string
		at   bool
	}{
		{"typed in pieces", adapter.Claude, []string{"/cl", "ear", "\r"}, true},
		{"one read", adapter.Claude, []string{"/clear\r"}, true},
		{"/compact", adapter.Claude, []string{"/compact\r"}, false},
		{"arguments", adapter.Claude, []string{"/clear old\r"}, false},
		{"a cursor key", adapter.Claude, []string{"/clear", "\x1b[D", "\r"}, false},
		{"an edit", adapter.Claude, []string{"/clearx", "\x7f", "\r"}, false},
		{"a paste", adapter.Claude, []string{"\x1b[200~/clear\x1b[201~", "\r"}, false},
		{"text after it, one read", adapter.Claude, []string{"/clear\rhi\r"}, false},
		{"an @ too", adapter.Claude, []string{"/clear @a\r"}, false},
		{"not leading", adapter.Claude, []string{"x/clear\r"}, false},
		{"Codex has none", adapter.Codex, []string{"/clear\r"}, false},
		{"Grok has none", adapter.Grok, []string{"/clear\r"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := turned(tt.v)
			for _, k := range tt.keys {
				s.send([]byte(k))
			}
			if s.atPrompt != tt.at {
				t.Errorf("atPrompt = %v, want %v", s.atPrompt, tt.at)
			}
		})
	}
	t.Run("mid-turn: still no prompt", func(t *testing.T) {
		s := turned(adapter.Claude)
		s.send([]byte("go\r"))
		s.send([]byte("/clear\r"))
		if s.atPrompt {
			t.Error("a command sent during a turn made the prompt free")
		}
	})
}

// TestTellWaitsHoldsNoOutput (T6897): a CLI that keeps drawing is running: no note.
func TestTellWaitsHoldsNoOutput(t *testing.T) {
	ts, c, done, _ := switchRunTurned(t, adapter.Claude, func(*Options) { waitNoteQuiet = 200 * time.Millisecond })
	ts.typeIn(t, "go\r")
	c.waitGot(t, []byte("go\r"))
	seen := len(ts.screen())
	ts.typeIn(t, "\x1c2")
	since := func() []byte { return ts.screen()[seen:] }
	poll(t, "the list to close", []byte(altScreenOff), since)
	seen = len(ts.screen())
	stop := time.After(600 * time.Millisecond)
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-time.After(50 * time.Millisecond):
			c.print(".")
		}
	}
	if bytes.Contains(since(), []byte("started a turn")) {
		t.Errorf("a note while the CLI drew: %q", since())
	}
	poll(t, "the note, once it is quiet", []byte("started a turn"), since)
	finish(t, c, done)
}

// TestTellWaitsLinesFit: each of the note's lines fits 80 columns.
func TestTellWaitsLinesFit(t *testing.T) {
	s := openCodeSession(t)
	t.Cleanup(s.closePicker)
	s.o.Vendor, s.sw.to, s.sw.picked = adapter.OpenCode, adapter.OpenCode, true
	s.lenKnown, s.popup = false, true
	old := waitNoteQuiet
	waitNoteQuiet = 0
	t.Cleanup(func() { waitNoteQuiet = old })
	s.tellWaits()
	if len(s.pk.lines) != 4 {
		t.Fatalf("lines = %q", s.pk.lines)
	}
	for _, l := range s.pk.lines {
		if len(l) > 80 {
			t.Errorf("%d columns: %q", len(l), l)
		}
	}
}

// TestOpenCodeClickIsNoInput (T6897, with T6932): a click on OpenCode is no typing.
// It goes to the CLI and leaves a hold and the tracker alone, however it arrives.
func TestOpenCodeClickIsNoInput(t *testing.T) {
	const click = "\x1b[<0;10;10M"
	for _, tt := range []struct {
		name string
		do   func(*session)
	}{
		{"from the filter", func(s *session) { s.pointer([]byte(click)) }},
		{"through send", func(s *session) { s.send([]byte(click)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := openCodeSession(t)
			s.held = true
			tt.do(s)
			if !s.held || s.inputSinceTurn || s.moved || s.dirty {
				t.Errorf("held %v, inputSinceTurn %v, moved %v, dirty %v", s.held, s.inputSinceTurn, s.moved, s.dirty)
			}
			if got := sentTo(s); got != click {
				t.Errorf("sent %q", got)
			}
		})
	}
}

// TestClickInExitGap (T6897, with T6932): a click between /exit and its Enter
// waits with the keys, so that Enter is not sent behind it, on either path.
func TestClickInExitGap(t *testing.T) {
	const click = "\x1b[<0;10;10M"
	for _, tt := range []struct {
		name string
		do   func(*session)
	}{
		{"from the filter", func(s *session) { s.pointer([]byte(click)) }},
		{"through send", func(s *session) { s.send([]byte(click)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := gapSession(t)
			tt.do(s)
			if string(s.gapKeys) != click {
				t.Fatalf("gapKeys = %q", s.gapKeys)
			}
			s.hookEvent(hookEvent{name: exitEnterCheck})
			if s.sw.to != "" {
				t.Errorf("the switch stands: %+v", s.sw)
			}
			got := sentTo(s)
			if strings.Contains(got, "\r") {
				t.Errorf("Enter sent behind the click: %q", got)
			}
			if !strings.HasSuffix(got, click) {
				t.Errorf("the click was lost: %q", got)
			}
		})
	}
}

// TestAskWinsOverHold (T6898, with T6932): an open ask holds too, and tellHeld's
// words offer an Enter that would choose Allow once, so the ask's own note comes
// first, and only an ask a turn end outlived is released, on a second pick.
func TestAskWinsOverHold(t *testing.T) {
	ask := hookEvent{name: hook.EventPermissionAsked, sessionID: "ses_a", request: "r1"}
	pick := func(s *session) string {
		t.Helper()
		s.pk.list()
		s.pk.vendors = []adapter.Vendor{adapter.Claude}
		s.pickerInput([]byte("1"))
		words := strings.Join(s.pk.lines, " ")
		s.closePicker()
		return words
	}
	newSession := func(t *testing.T) *session {
		s := openCodeSession(t)
		s.o.Vendors, s.o.CanSwitch = []adapter.Vendor{adapter.Claude}, func(adapter.Vendor) error { return nil }
		s.hookEvent(ask)
		if !s.held || !s.askOpen() {
			t.Fatalf("held %v, askOpen %v", s.held, s.askOpen())
		}
		return s
	}
	t.Run("before a turn end, no pick releases it", func(t *testing.T) {
		s := newSession(t)
		for range 3 {
			words := pick(s)
			if !strings.Contains(words, "permission or a question") || strings.Contains(words, "empty prompt") {
				t.Fatalf("words: %q", words)
			}
		}
		if !s.askOpen() || !s.held {
			t.Errorf("released: asked %v, held %v", s.asked, s.held)
		}
	})
	t.Run("a turn end outlived it: told, then picked again", func(t *testing.T) {
		s := newSession(t)
		s.hookEvent(hookEvent{name: "Stop", sessionID: "ses_a"})
		if !s.askStale {
			t.Fatal("not stale")
		}
		if words := pick(s); !strings.Contains(words, "pick claude again") {
			t.Fatalf("words: %q", words)
		}
		if !s.askOpen() {
			t.Fatal("released by the first pick")
		}
		pick(s)
		if s.askOpen() || s.held {
			t.Errorf("not released: asked %v, held %v", s.asked, s.held)
		}
	})
}

// TestOpenCodeNeverFollows (T6901, with T6932): OpenCode has no SessionStart, so
// one sent to its socket moves nothing, whatever it says and wherever it ran.
func TestOpenCodeNeverFollows(t *testing.T) {
	for _, source := range []string{"clear", "resume", "fork", "startup", ""} {
		if sessionMoves(adapter.OpenCode, source) {
			t.Errorf("source %q moves", source)
		}
	}
	s := openCodeSession(t)
	called := false
	s.o.OnFollow = func(string, string) { called = true }
	s.hookEvent(hookEvent{name: "Stop", sessionID: "ses_a", chain: ownChain})
	s.hookEvent(hookEvent{name: "SessionStart", sessionID: "ses_b", source: "clear", chain: ownChain2})
	if s.followed != "" || called || s.moveRefused {
		t.Errorf("followed %q, called %v, moveRefused %v", s.followed, called, s.moveRefused)
	}
}
