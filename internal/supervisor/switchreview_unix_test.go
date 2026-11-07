//go:build unix

package supervisor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
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
		{"Esc, Esc, Enter on an empty composer", []string{"\x1b", "\x1b", "\r"}},
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

// TestPickAfterMoveSays (T6901): after /clear no turn end comes, so a first pick
// says what it waits for, and picking again goes on, without spending a turn.
func TestPickAfterMoveSays(t *testing.T) {
	ts, c, done, sock := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "/clear\r")
	c.waitGot(t, []byte("/clear\r"))
	_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"SessionStart","payload":{"session_id":%q,"source":"clear"}}`, movedSID))
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "\x1c2")
	poll(t, "the note", []byte("moved to another session"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(escTimeout + 60*time.Millisecond)
	seen := len(ts.screen())
	since := func() []byte { return ts.screen()[seen:] }
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("Switching to"), since)
	ts.typeIn(t, "2")
	poll(t, "the status line", []byte("Esc stays"), since)
	finish(t, c, done)
}

// TestTellMovedFits: the note's lines fit 80 columns for each vendor.
func TestTellMovedFits(t *testing.T) {
	for _, on := range adapter.Vendors() {
		for _, to := range adapter.Vendors() {
			for _, l := range movedLines(on, to) {
				if len(l) > 80 {
					t.Errorf("%d columns: %q", len(l), l)
				}
			}
		}
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
