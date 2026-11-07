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

// promptSess is a session past its first turn end, with the CLI at its prompt.
func promptSess(v adapter.Vendor) *session {
	s := &session{toChild: newByteQueue(maxQueued), lenKnown: true, atPrompt: true, quit: make(chan struct{}),
		o: Options{Vendor: v, SessionID: func() string { return childSID }}}
	s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
	return s
}

// noSettleGap makes a turn end at once count as evidence, for a test of something
// else on a session with no harness.
func noSettleGap(t *testing.T) {
	t.Helper()
	old := settleTurnGap
	settleTurnGap = 0
	t.Cleanup(func() { settleTurnGap = old })
}

// TestDraftIsKnownText (T6897): any characters counted since the last Enter are
// known text, popup or not, and clearing them leaves doubt, not a block.
func TestDraftIsKnownText(t *testing.T) {
	for _, tt := range []struct {
		name  string
		keys  []string
		known bool
		doubt bool
	}{
		{"a draft with an @, no Enter", []string{"h", "i", " ", "@", "b", "o", "b"}, true, false},
		{"a draft after /cost's Enter", []string{"/cost\r", "h", "e", "l", "l", "o"}, true, false},
		{"a draft after an @ Enter with no turn", []string{"@fi\r", "h", "e", "y"}, true, false},
		{"a half-typed command", []string{"/mod"}, true, false},
		{"text after two Escs", []string{"\x1b", "\x1b", "h", "i"}, true, false},
		{"a dialog's arrow is doubt", []string{"/model\r", "\x1b[B"}, false, true},
		{"a popup's Enter, nothing since", []string{"@fi\r"}, false, true},
		{"a popup's Enter, text, Backspace", []string{"@fi\r", "x", "\x7f"}, false, true},
		{"a draft with an @, Ctrl+U", []string{"hi @bob", "\x15"}, false, true},
		{"an Esc on a popup's Enter", []string{"@fi\r", "\x1b"}, false, true},
		{"a plain draft, Backspace to nothing", []string{"ab", "\x7f", "\x7f"}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := promptSess(adapter.Claude)
			for _, k := range tt.keys {
				s.send([]byte(k))
			}
			if s.knownText() != tt.known || s.doubt() != tt.doubt {
				t.Errorf("knownText=%v doubt=%v, want %v %v", s.knownText(), s.doubt(), tt.known, tt.doubt)
			}
		})
	}
}

// TestDraftBlocksExit (T6897): the exit command never reaches a draft, however
// often the vendor is picked. It used to reach it after a note and a confirm.
func TestDraftBlocksExit(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []string
	}{
		{"an @ draft, no Enter", []string{"hi @bob"}},
		{"after /cost, a draft", []string{"/cost\r", "hello"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, _ := switchRunTurned(t, adapter.Claude)
			for _, k := range tt.keys {
				ts.typeIn(t, k)
				c.waitGot(t, []byte(k))
				time.Sleep(70 * time.Millisecond)
			}
			ts.typeIn(t, "\x1c2")
			poll(t, "the unsent-text note", []byte("you have not sent"), ts.screen)
			ts.typeIn(t, "\x1b")
			time.Sleep(escTimeout + 60*time.Millisecond)
			seen := len(ts.screen())
			ts.typeIn(t, "\x1c")
			time.Sleep(60 * time.Millisecond)
			ts.typeIn(t, "2")
			time.Sleep(confirmGuard + 300*time.Millisecond)
			since := ts.screen()[seen:]
			if bytes.Contains(since, []byte("Esc stays")) {
				t.Errorf("a status line came over a draft: %q", since)
			}
			ts.typeIn(t, "\x1c")
			time.Sleep(100 * time.Millisecond)
			if bytes.Contains(c.received(), []byte("/exit")) {
				t.Errorf("the exit command reached the draft: %q", c.received())
			}
			ts.typeIn(t, "\x1b")
			time.Sleep(escTimeout + 60*time.Millisecond)
			finish(t, c, done)
		})
	}
}

// TestOnlyTextCounts (T6897): what a key does to the draft. A count must not fall
// while text is still there, and a key that is not text must not bring the
// unsent-text note.
func TestOnlyTextCounts(t *testing.T) {
	bs := func(n int) string { return strings.Repeat("\x7f", n) }
	const clean, known, doubt = "clean", "known", "doubt"
	for _, tt := range []struct {
		name string
		keys string // one read per byte, but for an escape sequence; see split
		want string
	}{
		// Text still there: the count holds.
		{"Ctrl+A, Backspace past it", "abc\x01" + bs(4), known},
		{"Left, Backspace past the start", "abc\x1b[D" + bs(6), known},
		{"Home, Backspace", "abc\x1b[H" + bs(6), known},
		{"Up, Backspaces", "\x1b[A" + bs(3), known},
		{"Left, Ctrl+U", "abc\x1b[D\x15", known},
		{"Ctrl+J, Ctrl+U", "one\ntwo\x15", known},
		{"Esc and CR, Ctrl+U", "one\x1b\rtwo\x15", known},
		{"backslash and CR, Ctrl+U", "one\\\rtwo\x15", known},
		{"a pasted newline, Ctrl+U", "\x1b[200~one\ntwo\x1b[201~\x15", known},
		{"Ctrl+U, yank", "abc\x15\x19", known},
		{"Ctrl+U, undo", "abc\x15\x1f", known},
		{"a paste holding Ctrl+U", "\x1b[200~abc\x15\x1b[201~", known},
		{"a paste holding DELs", "\x1b[200~ab" + bs(4) + "\x1b[201~", known},
		{"Ctrl+W", "abc\x17", known},
		{"Ctrl+W, Backspaces", "abc\x17" + bs(3), known},
		{"Tab, Backspaces", "ab\t" + bs(2), known},
		{"Ctrl+C keeps a draft", "abc\x03", known},
		// Not text.
		{"Shift+Tab", "\x1b[Z", clean},
		{"Shift+Tab, CSI u", "\x1b[9;2u", clean},
		{"Page Up", "\x1b[5~", clean},
		{"Page Down", "\x1b[6~", clean},
		{"Ctrl+O", "\x0f", clean},
		{"Ctrl+C", "\x03", clean},
		{"Shift+Tab, then a draft and its Backspaces", "\x1b[Zab" + bs(2), clean},
		// Keys aggy cannot judge, with nothing counted.
		{"Up, Down", "\x1b[A\x1b[B", doubt},
		{"Up in application mode", "\x1bOA", doubt},
		{"a function key", "\x1b[15~", doubt},
		{"a private kitty key", "\x1b[57399u", doubt},
		{"Alt+b", "\x1bb", doubt},
		{"Tab", "\t", doubt},
		{"/model, arrow, Esc", "/model\r\x1b[B\x1b", doubt},
		// Text, in other forms.
		{"é, Backspace", "é\x7f", clean},
		{"é", "é", known},
		{"an emoji, Backspace", "😀\x7f", clean},
		{"kitty text", "\x1b[97u", known},
		{"kitty Shift+a", "\x1b[97;2u", known},
		{"kitty text with Num Lock", "\x1b[97;129u", known},
		{"kitty é", "\x1b[233u", known},
		{"modifyOtherKeys text", "\x1b[27;2;65~", known},
		{"kitty text, then its Backspace", "\x1b[97u\x1b[127u", clean},
		{"kitty Ctrl+U", "abc\x1b[117;5u", doubt},
		{"kitty Ctrl+U after Left", "abc\x1b[D\x1b[117;5u", known},
		{"kitty Alt+a", "\x1b[97;3u", doubt},
		{"kitty release", "\x1b[97;1:3u", clean},
		// Enter, in other forms.
		{"kitty Enter", "abc\x1b[13u", clean},
		{"kitty Enter twice", "\x1b[13u\x1b[13u", clean},
		{"modifyOtherKeys Enter", "abc\x1b[27;1;13~", clean},
		{"kitty Shift+Enter is a newline", "abc\x1b[13;2u", known},
		{"an Enter ends the blind", "abc\x1b[D\rxy" + bs(2), clean},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := promptSess(adapter.Claude)
			for _, k := range splitKeys(tt.keys) {
				s.send([]byte(k))
			}
			got := clean
			switch {
			case s.knownText():
				got = known
			case s.doubt():
				got = doubt
			}
			if got != tt.want {
				t.Errorf("%s, want %s (draftLen %d, blind %v)", got, tt.want, s.draftLen, s.draftBlind)
			}
		})
	}
}

// splitKeys cuts keys into reads as a person types them: one per key, with an
// escape sequence or a paste whole. A UTF-8 character is cut, as a read may be.
func splitKeys(keys string) []string {
	var out []string
	for i := 0; i < len(keys); {
		rest := []byte(keys[i:])
		n := 1
		switch {
		case bytes.HasPrefix(rest, pasteStart):
			n = bytes.Index(rest, pasteEnd) + len(pasteEnd)
		case rest[0] == esc && len(rest) > 1 && rest[1] == '\r':
			n = 2 // Option+Enter
		case rest[0] == esc:
			n = seqLen(rest)
		}
		out = append(out, keys[i:i+n])
		i += n
	}
	return out
}

// TestCSIEnterCounts (T6897): an Enter as CSI u is an Enter for the turn too.
func TestCSIEnterCounts(t *testing.T) {
	s := promptSess(adapter.Claude)
	s.send([]byte("go"))
	s.send([]byte("\x1b[13u"))
	if s.atPrompt {
		t.Errorf("an Enter left the CLI at its prompt")
	}
	s.send([]byte("more"))
	s.send([]byte("\x1b[13u"))
	if s.queued != 1 {
		t.Errorf("a message sent during a turn: queued %d", s.queued)
	}
	s = promptSess(adapter.Claude)
	s.send([]byte("\x1b[97;1:3u"))
	s.send([]byte("\x1b[13;1:3u"))
	if !s.atPrompt || s.inputSinceTurn {
		t.Errorf("a release is no key: atPrompt %v, input %v", s.atPrompt, s.inputSinceTurn)
	}
}

// TestDraftEditsBlockExit (T6897): the exit command never lands on what a Ctrl+U or
// Backspace left, and Ctrl+U and an Enter still get out.
func TestDraftEditsBlockExit(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys []string
	}{
		{"two lines, Ctrl+U", []string{"one", "\n", "two", "\x15"}},
		{"Left, Ctrl+U", []string{"abc", "\x1b[D", "\x15"}},
		{"Ctrl+A, Backspaces", []string{"abc", "\x01", "\x7f", "\x7f", "\x7f", "\x7f"}},
		{"Up, Backspaces", []string{"\x1b[A", "\x7f", "\x7f", "\x7f"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, _ := switchRunTurned(t, adapter.Claude)
			for _, k := range tt.keys {
				ts.typeIn(t, k)
				c.waitGot(t, []byte(k))
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(70 * time.Millisecond)
			ts.typeIn(t, "\x1c2")
			poll(t, "the unsent-text note", []byte("you have not sent"), ts.screen)
			ts.typeIn(t, "\x1b")
			time.Sleep(escTimeout + 60*time.Millisecond)
			seen := len(ts.screen())
			ts.typeIn(t, "\x1c")
			time.Sleep(60 * time.Millisecond)
			ts.typeIn(t, "2")
			since := func() []byte { return ts.screen()[seen:] }
			poll(t, "the note again", []byte("you have not sent"), since)
			if bytes.Contains(since(), []byte("Esc stays")) {
				t.Errorf("a status line came over the text: %q", since())
			}
			ts.typeIn(t, "\x1b")
			time.Sleep(escTimeout + 60*time.Millisecond)
			if bytes.Contains(c.received(), []byte("/exit")) {
				t.Fatalf("the exit command reached the text: %q", c.received())
			}
			// The way out: Ctrl+U and an Enter, then the pick that says no turn runs.
			ts.typeIn(t, "\x15")
			time.Sleep(70 * time.Millisecond)
			ts.typeIn(t, "\r")
			time.Sleep(70 * time.Millisecond)
			seen = len(ts.screen())
			ts.typeIn(t, "\x1c")
			time.Sleep(60 * time.Millisecond)
			ts.typeIn(t, "2")
			poll(t, "the status line", []byte("Esc stays"), since)
			time.Sleep(confirmGuard + 50*time.Millisecond)
			ts.typeIn(t, "\x1c")
			c.waitGot(t, []byte("/exit\r"))
			c.quit(0)
			wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})
		})
	}
}

// TestFirstPickSaysAgain (T6897): a draft, its note, the note's Esc, the list's Esc
// that cancels the switch, and a fresh pick: it says so again, not nothing.
func TestFirstPickSaysAgain(t *testing.T) {
	ts, c, done, _ := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "plain")
	c.waitGot(t, []byte("plain"))
	time.Sleep(70 * time.Millisecond)
	ts.typeIn(t, "\x1c2")
	poll(t, "the note", []byte("you have not sent"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(escTimeout + 60*time.Millisecond)
	ts.typeIn(t, "\x1c")
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "\x1b")
	time.Sleep(escTimeout + 60*time.Millisecond)
	seen := len(ts.screen())
	ts.typeIn(t, "\x1c")
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "2")
	poll(t, "the note", []byte("you have not sent"), func() []byte { return ts.screen()[seen:] })
	ts.typeIn(t, "\x1b")
	time.Sleep(escTimeout + 60*time.Millisecond)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Errorf("typed: %q", c.received())
	}
	finish(t, c, done)
}

// TestSettleNeedsAGap (T6897): a turn end clears doubt only a second or more after
// the last Enter, at the real gap: sooner it may be an interrupted turn's late Stop.
func TestSettleNeedsAGap(t *testing.T) {
	stop := hookEvent{name: "Stop", sessionID: childSID, chain: ownChain}
	gap := settleTurnGap + 100*time.Millisecond
	for _, tt := range []struct {
		name  string
		v     adapter.Vendor
		keys  []string
		wait  time.Duration
		end   hookEvent
		doubt bool
	}{
		{"a popup's Enter, a Stop at once", adapter.Claude, []string{"fix @a.g\r"}, 0, stop, true},
		{"a popup's Enter, a StopFailure at once", adapter.Claude, []string{"fix @a.g\r"}, 0, hookEvent{name: "StopFailure", sessionID: childSID, chain: ownChain}, true},
		{"a popup's Enter, a nested CLI's Stop", adapter.Claude, []string{"fix @a.g\r"}, gap, hookEvent{name: "Stop", sessionID: nestedSID, chain: nestedChain}, true},
		{"interrupted, Esc doubt, a late Stop", adapter.Claude, []string{"go\r", "\x1b", "\x1b", "\x1b", "\r"}, 0, stop, true},
		{"Codex's backtrack, a late Stop", adapter.Codex, []string{"\x1b", "\x1b", "h", "\r"}, 0, stop, true},
		{"a popup's Enter, then the second sends, at once", adapter.Claude, []string{"fix @a.g\r", "\r"}, 0, stop, true},
		{"a popup's Enter, then the second sends, a turn later", adapter.Claude, []string{"fix @a.g\r", "\r"}, gap, stop, false},
		{"a popup's Enter, text, send, a turn later", adapter.Claude, []string{"fix @a.g\r", "please\r"}, gap, stop, false},
		{"a popup's Enter at the prompt, a turn later", adapter.Claude, []string{"fix @a.go please\r"}, gap, stop, false},
		{"Esc doubt, a turn later", adapter.Claude, []string{"go\r", "\x1b", "\x1b", "\x1b", "\r"}, gap, stop, false},
		{"mid-turn popup's Enter is no evidence", adapter.Claude, []string{"go\r", "@fi\r"}, gap, stop, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := promptSess(tt.v)
			for _, k := range tt.keys {
				s.send([]byte(k))
				time.Sleep(55 * time.Millisecond) // an Esc, then its next key, as a person types
			}
			time.Sleep(tt.wait)
			s.hookEvent(tt.end)
			if s.doubt() != tt.doubt {
				t.Errorf("doubt=%v, want %v (known=%v)", s.doubt(), tt.doubt, s.knownText())
			}
		})
	}
}

// TestToldDoubtTurnEndAsks (T6897): the list promised a switch at the turn end and
// told of the doubt, so that turn end brings the status line, not silence for good:
// in the doubt's words if it stands, plain if the turn end settled it.
func TestToldDoubtTurnEndAsks(t *testing.T) {
	compact := fmt.Sprintf(`{"type":"hook-event","vendor":"claude","event":"SessionStart","payload":{"session_id":%q,"source":"compact"}}`, childSID)
	for _, tt := range []struct {
		name  string
		keys  []string
		hook  string
		stops int
		words bool
	}{
		{"/compact, a message during its turn: settled", []string{"/compact\r", "hello\r"}, compact, 1, false},
		{"a popup's Enter during a turn: it stands", []string{"go\r", "@fi\r"}, "", 2, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, sock := switchRunTurned(t, adapter.Claude)
			for i, k := range tt.keys {
				ts.typeIn(t, k)
				c.waitGot(t, []byte(k))
				time.Sleep(70 * time.Millisecond)
				if i == 0 && tt.hook != "" {
					_ = request(t, sock, tt.hook)
				}
			}
			ts.typeIn(t, "\x1c")
			poll(t, "the list's doubt", []byte("cannot tell if the prompt is empty"), ts.screen)
			ts.typeIn(t, "2")
			poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			for range tt.stops {
				_ = request(t, sock, stopLine)
				time.Sleep(60 * time.Millisecond)
			}
			poll(t, "the status line", []byte("Esc stays"), ts.screen)
			if got := bytes.Contains(ts.screen(), []byte("can't tell")); got != tt.words {
				t.Errorf("the status line's doubt: %v, want %v: %q", got, tt.words, ts.screen())
			}
			ts.typeIn(t, "\x1b")
			time.Sleep(escTimeout + 60*time.Millisecond)
			finish(t, c, done)
		})
	}
}

// TestUpCountsAsQueued (T6897): Up and an Enter during a turn may send a line from
// history, so the first turn end does not mean the prompt is free. Other cursor
// keys are not text: Down and an Enter answer an approval's menu.
func TestUpCountsAsQueued(t *testing.T) {
	for _, tt := range []struct {
		name   string
		keys   []string
		queued bool
	}{
		{"Up, Enter", []string{"go\r", "\x1b[A", "\r"}, true},
		{"Up in application mode, Enter", []string{"go\r", "\x1bOA", "\r"}, true},
		{"Down, Enter", []string{"go\r", "\x1b[B", "\r"}, false},
		{"Shift+Up, Enter", []string{"go\r", "\x1b[1;2A", "\r"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := promptSess(adapter.Claude)
			for _, k := range tt.keys {
				s.send([]byte(k))
			}
			s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
			if s.atPrompt == tt.queued {
				t.Errorf("at the first Stop: atPrompt=%v queued=%d, want queued=%v", s.atPrompt, s.queued, tt.queued)
			}
		})
	}
}

// TestDoubtLineFits (T6897): the doubt's status line keeps its words, its keys, and
// the command it types within 80 columns, whatever the vendor, with new and brief
// on. It used to end before saying it types /exit.
func TestDoubtLineFits(t *testing.T) {
	for _, tt := range []struct {
		name, keys, to string
		on             adapter.Vendor
	}{
		{"a short vendor, no options", "2", "grok", adapter.Claude},
		{"a short vendor, new and brief", "bn2", "grok", adapter.Claude},
		{"a long vendor, no options", "1", "codex", adapter.Claude},
		{"a long vendor, new and brief", "bn1", "codex", adapter.Claude},
		{"the longest vendor, new and brief", "bn1", "claude", adapter.Codex},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, _ := switchRunTurned(t, tt.on)
			ts.typeIn(t, "@fi\r") // no turn end follows, and the note needs a prompt
			c.waitGot(t, []byte("@fi\r"))
			time.Sleep(70 * time.Millisecond)
			ts.typeIn(t, "\x1c")
			time.Sleep(60 * time.Millisecond)
			ts.typeIn(t, tt.keys)
			poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			time.Sleep(100 * time.Millisecond)
			seen := len(ts.screen())
			ts.typeIn(t, "\x1c")
			time.Sleep(60 * time.Millisecond)
			ts.typeIn(t, tt.keys)
			since := func() []byte { return ts.screen()[seen:] }
			poll(t, "the status line", []byte("Esc stays"), since)
			out := string(since())
			out = out[strings.LastIndex(out, "aggy: "):]
			if i := strings.IndexByte(out, esc); i >= 0 {
				out = out[:i]
			}
			if len(out) > 79 {
				t.Errorf("%d columns wide: %q", len(out), out)
			}
			if !strings.HasSuffix(out, keysEnd) {
				t.Errorf("does not end at its keys: %q", out)
			}
			for _, w := range []string{"can't tell", "-> " + tt.to, "/exit", "Esc stays"} {
				if !strings.Contains(out, w) {
					t.Errorf("%q is missing from %q", w, out)
				}
			}
			ts.typeIn(t, "\x1b")
			time.Sleep(escTimeout + 60*time.Millisecond)
			finish(t, c, done)
		})
	}
}

// TestListedNameSkillLimit pins a known limit, not a wanted behavior: a skill named
// like a local command runs a turn aggy counts as none, so the doubt note opens and
// holds its output until a key. TODO(T6895): a live check settles whether a skill
// can take a built-in's name; if it can, this must flow as
// TestOutputFlowsThroughTheWait does.
func TestListedNameSkillLimit(t *testing.T) {
	ts, c, done, _ := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "/status\r")
	c.waitGot(t, []byte("/status\r"))
	time.Sleep(100 * time.Millisecond)
	ts.typeIn(t, "\x1c2")
	poll(t, "the note", []byte("prompt is empty"), ts.screen)
	c.print("TURNOUTPUT")
	time.Sleep(200 * time.Millisecond)
	if bytes.Contains(ts.screen(), []byte("TURNOUTPUT")) {
		t.Errorf("the note no longer holds output: this limit may be gone, so update docs/design.md")
	}
	ts.typeIn(t, "\x1b")
	poll(t, "the output, after the key", []byte("TURNOUTPUT"), ts.screen)
	finish(t, c, done)
}

// TestDraftDuringSwitchCheck (T6897): SwitchBlocked runs after the confirm and
// before the exit command. A draft typed while it runs is refused there, as text.
func TestDraftDuringSwitchCheck(t *testing.T) {
	release := make(chan struct{})
	called := make(chan struct{}, 1)
	ts, c, done, _ := switchRunTurned(t, adapter.Claude, func(o *Options) {
		o.SwitchBlocked = func(adapter.Vendor, string) string {
			called <- struct{}{}
			<-release
			return ""
		}
	})
	ts.typeIn(t, "\x1c2")
	poll(t, "the status line", []byte("Esc stays"), ts.screen)
	time.Sleep(confirmGuard + 50*time.Millisecond)
	ts.typeIn(t, "\x1c")
	select {
	case <-called:
	case <-time.After(waitLimit):
		t.Fatal("SwitchBlocked did not run after the confirm")
	}
	ts.typeIn(t, "draft")
	c.waitGot(t, []byte("draft"))
	time.Sleep(70 * time.Millisecond)
	close(release)
	time.Sleep(200 * time.Millisecond)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Errorf("the exit command reached the draft: %q", c.received())
	}
	finish(t, c, done)
}
