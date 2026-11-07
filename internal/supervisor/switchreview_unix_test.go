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
			ts, c, done, _ := switchRunTurned(t, adapter.Codex)
			for _, k := range tt.keys {
				ts.typeIn(t, k)
				c.waitGot(t, []byte(k))
				time.Sleep(70 * time.Millisecond)
			}
			// The Enter may have started a turn, so the list says that and the
			// doubt, and nothing is held; the next pick says none runs and confirms.
			ts.typeIn(t, "\x1c")
			poll(t, "the list's words", []byte("cannot tell if the prompt is empty"), ts.screen)
			ts.typeIn(t, "1")
			poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			time.Sleep(100 * time.Millisecond)
			if bytes.Contains(c.received(), []byte("/exit")) {
				t.Fatalf("typed before any confirm: %q", c.received())
			}
			seen := len(ts.screen())
			ts.typeIn(t, "\x1c")
			time.Sleep(60 * time.Millisecond)
			ts.typeIn(t, "1")
			since := func() []byte { return ts.screen()[seen:] }
			poll(t, "the status line", []byte("can't tell if prompt is empty"), since)
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
	noSettleGap(t)
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
	noSettleGap(t) // TestSettleNeedsAGap has the gap
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
		{"input since the Enter is known", []string{"@fi\r", "x"}, []hookEvent{stop}, false, true},
		{"a key since the Enter is known", []string{"@fi\r", "\x1b[A"}, []hookEvent{stop}, false, true},
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
		waits = "list said so; waits"
		both  = "list said so, and cannot tell; waits"
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
		{"/compact, a message, turn end", []step{k("/compact\r"), C, k("hello\r"), S}, false, line, ""},
		{"/compact, a message, turn end, real timing", []step{k("/compact\r"), C, k("hello\r"), S}, true, waits, line},
		{"/model closed with Esc, a message", []step{k("/model\r"), k("\x1b"), k("hello\r"), S}, false, line, ""},
		{"/model, real timing", []step{k("/model\r"), k("\x1b"), k("hello\r"), S}, true, line, ""},
		{"/model still open", []step{k("/model\r")}, false, doubt, line},
		{"/cost, 2 messages", []step{k("/cost\r"), k("one\r"), S, k("two\r"), S}, false, line, ""},
		{"/cost, 2 messages, real timing", []step{k("/cost\r"), k("one\r"), S, k("two\r"), S}, true, line, ""},
		{"/status, nothing else", []step{k("/status\r")}, false, doubt, line},
		{"a custom command, no turn end", []step{k("/mine\r")}, false, both, line},
		{"@file in a message", []step{k("fix @a.go please\r"), S}, false, line, ""},
		{"@file, no turn end", []step{k("@fi\r")}, false, both, line},
		{"@file, Enter accepts, Enter sends, turn end", []step{k("fix @a.g\r"), k("\r"), S}, false, line, ""},
		{"@file, Enter accepts, Enter sends, real timing", []step{k("fix @a.g\r"), k("\r"), S}, true, line, ""},
		{"@file, Enter accepts, more text, send, turn end", []step{k("fix @a.g\r"), k("please\r"), S}, false, line, ""},
		{"@file, Enter accepts, more text, real timing", []step{k("fix @a.g\r"), k("please\r"), S}, true, waits, line},
		{"a message, no turn end yet", []step{k("go\r")}, false, waits, line},
		{"Esc interrupts, a message", []step{k("go\r"), k("\x1b"), k("hello\r"), S}, false, line, ""},
		{"Esc interrupts, a message, real timing", []step{k("go\r"), k("\x1b"), k("hello\r"), S}, true, waits, line},
		{"a message sent during a turn, real timing", []step{k("go\r"), k("more\r"), S}, true, waits, line},
		{"Esc on an empty prompt", []step{k("\x1b")}, false, doubt, line},
		{"Esc interrupts, Up, Enter", []step{k("go\r"), k("\x1b"), k("\x1b[A"), k("\r"), S}, false, line, ""},
		{"Esc interrupts, Up, Enter, real timing", []step{k("go\r"), k("\x1b"), k("\x1b[A"), k("\r"), S}, true, waits, line},
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
				queuedQuiet = 50 * time.Millisecond
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
			// What the list said, and what shows after it closes: a note would
			// come back to the alternate screen, so it shows in what follows.
			shown := func(seen int) string {
				out := ts.screen()[seen:]
				list, after := out, []byte(nil)
				if i := bytes.Index(out, []byte(altScreenOff)); i >= 0 {
					list, after = out[:i], out[i:]
				}
				has := func(w string) bool { return bytes.Contains(after, []byte(w)) }
				switch {
				case has("Esc stays"):
					return line
				case has("have not sent"):
					return "note: unsent"
				case has("prompt is empty"):
					return doubt
				case has("waiting for you"):
					return "note: held"
				case bytes.Contains(list, []byte("cannot tell if the prompt is empty")):
					return both
				case bytes.Contains(list, []byte("A turn may be running")):
					return waits
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
				if got != waits && got != both { // no note up: an Esc would reach the CLI
					ts.typeIn(t, "\x1b")
					time.Sleep(escTimeout + 60*time.Millisecond)
				}
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
	ts, c, done, sock := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "/compact\r")
	c.waitGot(t, []byte("/compact\r"))
	_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"SessionStart","payload":{"session_id":%q,"source":"compact"}}`, childSID))
	time.Sleep(70 * time.Millisecond)
	ts.typeIn(t, "\x1c")
	poll(t, "the list's words", []byte("cannot tell if the prompt is empty"), ts.screen)
	ts.typeIn(t, "2")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	time.Sleep(60 * time.Millisecond)
	seen := len(ts.screen())
	since := func() []byte { return ts.screen()[seen:] }
	ts.typeIn(t, "\x1c")
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "2")
	poll(t, "the status line", []byte("can't tell if prompt is empty"), since)
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

// TestOutputFlowsThroughTheWait (T6897): a pick that waits holds nothing. The list
// has said why, so after it closes no note opens, however long the CLI is silent,
// and all it draws reaches the terminal, at the real queuedQuiet or short of it.
func TestOutputFlowsThroughTheWait(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []string
		stop bool
	}{
		{"an Enter may have started a turn", []string{"go\r"}, false},
		{"a message queued, real timing", []string{"go\r", "more\r"}, true},
		{"the prompt in doubt too", []string{"fix @a.go\r"}, false},
		{"Up, Enter during a turn, real timing", []string{"go\r", "\x1b[A", "\r"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, sock := switchRunTurned(t, adapter.Claude, func(*Options) { queuedQuiet = realQueuedQuiet })
			for _, k := range tt.keys {
				ts.typeIn(t, k)
				c.waitGot(t, []byte(k))
			}
			if tt.stop {
				_ = request(t, sock, stopLine)
				time.Sleep(60 * time.Millisecond)
			}
			ts.typeIn(t, "\x1c")
			poll(t, "the list's words", []byte("A turn may be running in claude"), ts.screen)
			ts.typeIn(t, "2")
			poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			seen := len(ts.screen())
			since := func() []byte { return ts.screen()[seen:] }
			// Silences past every quiet the supervisor knows of in this harness,
			// with drawing between them: each draw must come through at once.
			for i := range 4 {
				time.Sleep(300 * time.Millisecond)
				mark := fmt.Sprintf("DRAW%d", i)
				c.print(mark)
				poll(t, mark, []byte(mark), since)
			}
			if out := since(); bytes.Contains(out, []byte(altScreenOn)) || bytes.Contains(out, []byte("Esc stays")) {
				t.Errorf("something opened during the wait: %q", out)
			}
			finish(t, c, done)
		})
	}
}

// TestListSaysWhenAPickWaits (T6897): the list's words come only when a pick would
// wait, and a turn end while it is open blanks them, with the rows they took.
func TestListSaysWhenAPickWaits(t *testing.T) {
	var b bytes.Buffer
	p := &picker{mayRun: true, doubtSaid: true, on: adapter.OpenCode, vendors: []adapter.Vendor{adapter.Claude}}
	p.draw(&b, false)
	seen := 0
	for _, l := range strings.Split(b.String(), "\r\n") {
		if strings.Contains(l, "may be running") || strings.Contains(l, "pick the vendor again") || strings.Contains(l, "cannot tell") {
			seen++
			if len(l) > 80 {
				t.Errorf("%d columns: %q", len(l), l)
			}
		}
	}
	if seen != 3 {
		t.Errorf("%d lines: %q", seen, b.String())
	}
	ts, c, done, sock := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("brief: off"), ts.screen)
	if bytes.Contains(ts.screen(), []byte("may be running")) {
		t.Errorf("after a turn end, with no input: %q", ts.screen())
	}
	ts.typeIn(t, "\x1b")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	ts.typeIn(t, "go\r")
	c.waitGot(t, []byte("go\r"))
	seenAt := len(ts.screen())
	since := func() []byte { return ts.screen()[seenAt:] }
	ts.typeIn(t, "\x1c")
	poll(t, "the list's words", []byte("may be running"), since)
	seenAt = len(ts.screen())
	_ = request(t, sock, stopLine)
	poll(t, "the redraw", []byte("brief: off"), since)
	if bytes.Contains(since(), []byte("may be running")) {
		t.Errorf("the words stayed after the turn end: %q", since())
	}
	ts.typeIn(t, "\x1b")
	poll(t, "the list to close again", []byte(altScreenOff), since)
	finish(t, c, done)
}

// TestFollowLeavesTurnState (T6901): a move changes the id and nothing about a
// turn. Codex sends SessionStart inside the turn, so one may be running.
func TestFollowLeavesTurnState(t *testing.T) {
	s := turned(adapter.Codex)
	s.send([]byte("/clear"))
	s.send([]byte("\r"))
	s.send([]byte("do the work"))
	s.send([]byte("\r"))
	s.queued, s.queuedWait = 1, true
	s.hookEvent(hookEvent{name: "SessionStart", sessionID: movedSID, source: "clear", chain: ownChain2})
	if s.atPrompt || !s.mayBeRunning() || !s.inputSinceTurn {
		t.Errorf("atPrompt %v, mayBeRunning %v, inputSinceTurn %v", s.atPrompt, s.mayBeRunning(), s.inputSinceTurn)
	}
	if s.queued != 1 || !s.queuedWait {
		t.Errorf("queued %d, queuedWait %v", s.queued, s.queuedWait)
	}
	if !s.dirty {
		t.Error("a message after the command is not the command alone: the doubt must stay")
	}
	if got := s.o.SessionID(); got == "" || s.followed != movedSID {
		t.Errorf("followed %q", s.followed)
	}
}

// TestFollowMidTurnNoOffer (T6901): through Run, the status line does not come
// and /exit is not typed while that turn runs.
func TestFollowMidTurnNoOffer(t *testing.T) {
	ts, c, done, sock := switchRunTurned(t, adapter.Codex)
	ts.typeIn(t, "/clear\r")
	c.waitGot(t, []byte("/clear\r"))
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "do the work\r")
	c.waitGot(t, []byte("do the work\r"))
	ts.typeIn(t, "\x1c1")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"codex","event":"SessionStart","payload":{"session_id":%q,"source":"clear"}}`, movedSID))
	never(t, "an offer mid-turn", []byte("Esc stays"), ts.screen, 300*time.Millisecond)
	finish(t, c, done)
}

// TestFollowDoubtOnlyForCommand (T6901): the move clears only what its command
// left, not text around it, nor a newline taken for an Enter.
func TestFollowDoubtOnlyForCommand(t *testing.T) {
	for _, tt := range []struct {
		name  string
		keys  []string
		dirty bool
	}{
		{"only the command", []string{"/clear", "\r"}, false},
		{"in one read", []string{"/clear\r"}, false},
		{"a list after it", []string{"/resume", "\r", "\x1b[B", "\r"}, false},
		{"a draft after it", []string{"/resume", "\r", "abc"}, true},
		{"option+enter after a draft", []string{"/resume", "\r", "abc", "\x1b\r"}, true},
		{"backslash enter after a draft", []string{"/resume", "\r", "abc", "\\\r"}, true},
		{"a message after it, in one read", []string{"/clear\rdo it\r"}, true},
		{"a message after it", []string{"/clear", "\r", "do it", "\r"}, true},
		{"an @ in it", []string{"/clear @a", "\r"}, true},
		{"text before it", []string{"x /clear", "\r"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := turned(adapter.Claude)
			for _, k := range tt.keys {
				s.send([]byte(k))
			}
			s.hookEvent(hookEvent{name: "SessionStart", sessionID: movedSID, source: "resume", chain: ownChain2})
			if s.followed != movedSID {
				t.Fatalf("not followed")
			}
			if s.dirty != tt.dirty {
				t.Errorf("dirty = %v, want %v", s.dirty, tt.dirty)
			}
		})
	}
}

// TestRefusedMoveSays (T6901): a move refused leaves turn ends uncounted, so the
// list says so, with a way out that works, and never that none was seen.
func TestRefusedMoveSays(t *testing.T) {
	t.Run("no baseline: Codex reopened, /clear first", func(t *testing.T) {
		ts, c, done, sock := switchRunOn(t, false, adapter.Codex)
		ts.typeIn(t, "/clear\r")
		c.waitGot(t, []byte("/clear\r"))
		_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"codex","event":"SessionStart","payload":{"session_id":%q,"source":"clear"}}`, movedSID))
		_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":%q}}`, movedSID))
		time.Sleep(60 * time.Millisecond)
		ts.typeIn(t, "\x1c")
		poll(t, "the list", []byte("brief: off"), ts.screen)
		for _, want := range []string{"a session aggy could not confirm", "quit it yourself", "has counted no turn end"} {
			if !bytes.Contains(ts.screen(), []byte(want)) {
				t.Errorf("no %q: %q", want, ts.screen())
			}
		}
		if bytes.Contains(ts.screen(), []byte("has seen no turn end")) {
			t.Error("the list says no turn end was seen")
		}
		ts.typeIn(t, "\x1b")
		poll(t, "the list to close", []byte(altScreenOff), ts.screen)
		finish(t, c, done)
	})
	t.Run("a turn counted: picking twice is the way out", func(t *testing.T) {
		s := turned(adapter.Claude)
		s.hookEvent(hookEvent{name: "SessionStart", sessionID: nestedSID, source: "clear", chain: nestedChain})
		if !s.moveRefused {
			t.Fatal("not recorded")
		}
		var b bytes.Buffer
		p := &picker{moveRefused: true, on: adapter.Claude}
		p.draw(&b, false)
		if !strings.Contains(b.String(), "pick the vendor twice") {
			t.Errorf("list: %q", b.String())
		}
		for _, l := range strings.Split(b.String(), "\r\n") {
			if strings.Contains(l, "confirm") || strings.Contains(l, "may not count") {
				if len(l) > 80 {
					t.Errorf("%d columns: %q", len(l), l)
				}
			}
		}
		// A turn end naming the session on screen ends the doubt.
		s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
		if s.moveRefused {
			t.Error("a counted turn end left the words")
		}
	})
}

// TestHeldDeadlineFixed (T6897): keys wait for a split sequence 50 ms from its
// first byte, so key repeat after Alt+] cannot keep them back.
func TestHeldDeadlineFixed(t *testing.T) {
	ts, c, done, _ := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "\x1b]")
	start := time.Now()
	for time.Since(start) < 500*time.Millisecond {
		ts.typeIn(t, "a")
		time.Sleep(25 * time.Millisecond)
	}
	if !bytes.Contains(c.received(), []byte("a")) {
		t.Errorf("after %v of key repeat the CLI has %q", time.Since(start), c.received())
	}
	c.waitGot(t, []byte("aaaa"))
	finish(t, c, done)
}

// TestWaitingOptionsStayWithVendor (T6902): a waiting switch's options are that
// vendor's: a pick of another drops them, and one toggled here stays.
func TestWaitingOptionsStayWithVendor(t *testing.T) {
	ts, c, done, sock := switchRunTurned(t, adapter.Claude) // vendors: codex, grok
	ts.typeIn(t, "go\r")
	c.waitGot(t, []byte("go\r"))
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("brief: off"), ts.screen)
	ts.typeIn(t, "m")
	ts.typeIn(t, "gpt-5\r")
	poll(t, "the model", []byte("gpt-5"), ts.screen)
	ts.typeIn(t, "1") // codex
	time.Sleep(100 * time.Millisecond)
	seen := len(ts.screen())
	since := func() []byte { return ts.screen()[seen:] }
	ts.typeIn(t, "\x1c")
	poll(t, "the list again", []byte("gpt-5"), since)
	ts.typeIn(t, "2") // grok instead
	time.Sleep(100 * time.Millisecond)
	// From here on: the list itself still showed the model.
	seen = len(ts.screen())
	_ = request(t, sock, stopLine)
	poll(t, "the status line", []byte("Esc stays"), since)
	if bytes.Contains(since(), []byte("gpt-5")) {
		t.Errorf("the model for codex went with the pick of grok: %q", since())
	}
	finish(t, c, done)
}

// TestPickDropsOnlySeed: a toggle made in this list is for this pick.
func TestPickDropsOnlySeed(t *testing.T) {
	p := &picker{waiting: adapter.Codex, seed: Choice{Model: "gpt-5", New: true, Brief: true},
		opts: Choice{Model: "other", New: true, Brief: true}}
	_, c := p.choose(adapter.Grok)
	if c.Model != "other" || c.New || c.Brief {
		t.Errorf("other vendor: %+v", c)
	}
	p = &picker{waiting: adapter.Codex, seed: Choice{Model: "gpt-5", New: true},
		opts: Choice{Model: "gpt-5", New: true}}
	if _, c = p.choose(adapter.Codex); c.Model != "gpt-5" || !c.New {
		t.Errorf("same vendor: %+v", c)
	}
}

// TestRepickKeepsAgentTools (T6902): the list cannot set an agent's tools, so
// picking the same vendor again keeps them.
func TestRepickKeepsAgentTools(t *testing.T) {
	ts, c, done, sock := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "go\r")
	c.waitGot(t, []byte("go\r"))
	_ = request(t, sock, `{"type":"switch-request","vendor":"grok","tools":["context7"]}`)
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("brief: off"), ts.screen)
	time.Sleep(60 * time.Millisecond)
	seen := len(ts.screen())
	ts.typeIn(t, "2")
	since := func() []byte { return ts.screen()[seen:] }
	poll(t, "the status line", []byte("Esc stays"), since)
	if !bytes.Contains(since(), []byte("context7")) {
		t.Errorf("the agent's tool is gone from the offer: %q", since())
	}
	finish(t, c, done)
}

// TestStripFollowsPaste (T6897): a report-shaped paste at a prompt of aggy's is
// the paste's, opened and closed in one read or across reads.
func TestStripFollowsPaste(t *testing.T) {
	f := newKeyFilter(0x1c)
	rep, rest := f.strip([]byte("\x1b[200~\x1b[I1\x1b[201~"))
	if len(rep) != 0 || string(rest) != "\x1b[200~\x1b[I1\x1b[201~" {
		t.Errorf("reports %q, rest %q", rep, rest)
	}
	if f.inPaste {
		t.Error("still in the paste")
	}
	f = newKeyFilter(0x1c)
	_, _ = f.strip([]byte("\x1b[200~a"))
	if rep, _ = f.strip([]byte("\x1b[Ib")); len(rep) != 0 {
		t.Errorf("reports %q", rep)
	}
	_, _ = f.strip([]byte("\x1b[201~"))
	if f.inPaste {
		t.Error("the close marker was missed")
	}
	if rep, _ = f.strip([]byte("\x1b[I")); len(rep) == 0 {
		t.Error("a reply after the paste is not one")
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
