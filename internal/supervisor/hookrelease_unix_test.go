//go:build unix

package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// TestBlockedRecovers (#146 review, second and third rounds, HIGH): a prompt
// aggy blocked starts no turn, so no turn end comes to settle the tracker.
// Before, every command after a stray key was blocked too, with advice that
// did not work. Now the command typed in full, which the hook reports, acts at
// once (typedMatch), whatever aggy doubted before the line. After Left it waits
// for an Esc (the keys may be in Claude's other view): an Esc, and then either a
// counted turn end or the command typed in full again.
func TestBlockedRecovers(t *testing.T) {
	for _, c := range []struct {
		name    string
		version string
		keys    []string
		first   bool // the first /switch grok typed in full acts at once
		thenEsc bool // the person then presses Esc
		turnEnd bool // and a counted turn end comes
	}{
		{"Backspace, unchecked version", "", []string{kBS}, true, false, false},
		{"Up, unchecked version", "2.1.330", []string{kUp}, true, false, false},
		{"Left, unchecked version, then Esc and the line", "", []string{kLeft}, false, true, false},
		{"Left, unchecked version, then Esc and a turn end", "", []string{kLeft}, false, true, true},
		{"Left and Esc, checked version", checkedClaude, []string{kLeft, kEsc}, true, false, false},
		{"Left, checked version, then Esc and the line", checkedClaude, []string{kLeft}, false, true, false},
		{"Left, checked version, then Esc and a turn end", checkedClaude, []string{kLeft}, false, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSettleGap(t)
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.VendorVersion = c.version
			s.input = s.send
			for _, k := range c.keys {
				s.send([]byte(k))
			}
			typeLine(s, "/switch grok")
			err := forge(s, "/switch grok")
			if c.first {
				if err != nil || s.sw.to != adapter.Grok || !s.sw.keyed || s.sw.hooked {
					t.Fatalf("err %v switch %q keyed %v hooked %v: not acted at once (doubts %v)", err, s.sw.to, s.sw.keyed, s.sw.hooked, s.lineDoubts())
				}
				if want := adapter.BlockedPromptEmpty(adapter.Claude, c.version); s.sw.cleared != want {
					t.Errorf("one step %v, want %v: only the live fact that a blocked prompt leaves the composer empty allows it", s.sw.cleared, want)
				}
				if got := sentTo(s); strings.Contains(got, "\x15") {
					t.Errorf("a Ctrl+U went to the CLI: %q", got)
				}
				return
			}
			var be *blockedErr
			if !errors.Is(err, errAsked) && !errors.As(err, &be) {
				t.Fatalf("err %v: the prompt was not blocked", err)
			}
			if s.sw.to != "" || !s.sideView {
				t.Fatalf("acted with no Esc after Left: switch %q sideView %v", s.sw.to, s.sideView)
			}
			if s.open {
				s.pickerInput([]byte{esc}) // the question's Esc, which goes on to the CLI
			}
			if !c.thenEsc {
				return
			}
			s.send([]byte(kEsc))
			if !s.sideView {
				t.Fatal("an Esc alone cleared the other view")
			}
			if c.turnEnd {
				s.send([]byte("x\r")) // a message that runs a turn
				s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
				if s.sideView {
					t.Fatal("the counted turn end after the Esc did not clear the other view")
				}
			}
			s.toChild = newByteQueue(maxQueued)
			typeLine(s, "/switch grok")
			if s.sw.to != adapter.Grok {
				if err := forge(s, "/switch grok"); err != nil {
					t.Fatalf("err %v (doubts %v)", err, s.lineDoubts())
				}
			}
			if s.sw.to != adapter.Grok || s.sideView {
				t.Fatalf("switch %q sideView %v: not recovered", s.sw.to, s.sideView)
			}
		})
	}
}

// TestForgedChangesNothing (#146 review, third round, HIGH): a request alone
// never changes state. The quiet release let one clear a doubt after Claude's
// own dialog, during a turn's first seconds, and in Claude's other view. Now a
// request for a command other than the line typed, or for any line after Left
// with no Esc, or after a key since the Enter, runs nothing, types nothing, and
// leaves every doubt standing.
func TestForgedChangesNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		keys   func(s *session)
		forged string
		side   bool // the other view is open, and stays
	}{
		{"after a /model-style line", func(s *session) { s.send([]byte(kUp)); typeLine(s, "/model") }, "/switch grok", false},
		{"during a /review turn", func(s *session) { s.send([]byte(kUp)); typeLine(s, "/review") }, "/switch grok", false},
		{"another command than typed", func(s *session) { s.send([]byte(kUp)); typeLine(s, "/switch codex") }, "/switch grok", false},
		{"the line typed, a key since", func(s *session) { s.send([]byte(kUp)); typeLine(s, "/switch grok"); s.send([]byte(kDown)) }, "/switch grok", false},
		{"an edit in the line", func(s *session) {
			s.send([]byte(kUp))
			typeKeys(s, "/switch grokk")
			s.send([]byte(kBS))
			s.send([]byte("\r"))
		}, "/switch grok", false},
		{"a cursor key in the line", func(s *session) {
			s.send([]byte(kUp))
			typeKeys(s, "/switch grok")
			s.send([]byte(kDown))
			s.send([]byte("\r"))
		}, "/switch grok", false},
		{"a paste", func(s *session) {
			s.send([]byte(kUp))
			s.send([]byte("\x1b[200~/switch grok\x1b[201~"))
			s.send([]byte("\r"))
		}, "/switch grok", false},
		{"its Enter with the text", func(s *session) { s.send([]byte(kUp)); s.send([]byte("/switch grok\r")) }, "/switch grok", false},
		{"the line typed in the other view", func(s *session) { s.send([]byte(kLeft)); typeLine(s, "/switch grok") }, "/switch grok", true},
		{"a / line in the other view, then Esc", func(s *session) { s.send([]byte(kLeft)); typeLine(s, "/inbox"); s.send([]byte(kEsc)) }, "/inbox", true},
		{"Left, Esc, then a forged request", func(s *session) { s.send([]byte(kLeft)); s.send([]byte(kEsc)) }, "/switch grok", true},
	} {
		for _, version := range []string{"", checkedClaude} {
			t.Run(c.name+" "+version, func(t *testing.T) {
				s, ran := viewSess(t, adapter.Claude, "")
				s.o.VendorVersion = version
				s.input = s.send
				c.keys(s)
				s.toChild = newByteQueue(maxQueued)
				atPrompt, lenKnown := s.atPrompt, s.lenKnown
				err := forge(s, c.forged)
				var be *blockedErr
				if !errors.Is(err, errAsked) && !errors.As(err, &be) {
					t.Fatalf("err %v: the prompt was not blocked", err)
				}
				if s.open {
					s.pickerInput([]byte{esc})
				}
				if s.sw.to != "" || len(*ran) != 0 || s.pk.menu != nil || s.pk.page != nil {
					t.Fatalf("ran: switch %q runs %q", s.sw.to, *ran)
				}
				if got := sentTo(s); strings.Contains(got, "\x15") || strings.Contains(got, "/") {
					t.Errorf("typed %q into the CLI", got)
				}
				if s.atPrompt != atPrompt || s.lenKnown != lenKnown { // len_unknown kept after the Up
					t.Errorf("atPrompt %v -> %v, lenKnown %v -> %v", atPrompt, s.atPrompt, lenKnown, s.lenKnown)
				}
				if s.sideView != c.side {
					t.Errorf("sideView %v, want %v", s.sideView, c.side)
				}
			})
		}
	}
}

// TestPromptBlockedSideView (#146 review, third round): a blocked prompt's
// tidy-up clears Claude's other view only after an Esc; with none, the keys may
// still be there.
func TestPromptBlockedSideView(t *testing.T) {
	for _, c := range []struct {
		sideEsc, want bool
	}{{true, false}, {false, true}} {
		s, _ := viewSess(t, adapter.Claude, "")
		s.sideView, s.sideEsc = true, c.sideEsc
		s.promptBlocked()
		if s.sideView != c.want {
			t.Errorf("Esc %v: sideView %v, want %v", c.sideEsc, s.sideView, c.want)
		}
	}
}

// TestBlockNoteAdvice (#146 review): a block's note gives advice that works: the
// switch key for a switch; for another command, typing it in full, which
// typedMatch then acts on; after Left with no Esc, an Esc first.
func TestBlockNoteAdvice(t *testing.T) {
	for _, c := range []struct {
		name, key, line, want string
	}{
		{"a switch", kUp, "/switch grok", `Ctrl+\ opens the switch list`},
		{"another command", kUp, "/inbox", "Type it in full at claude's prompt"},
		{"in the other view", kLeft, "/inbox", "Press Esc, then type it in full"},
		{"a switch in the other view", kLeft, "/switch grok", `Ctrl+\ opens the switch list`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.send([]byte(c.key))
			typeLine(s, "/other")
			s.held = true // no question: a note
			var be *blockedErr
			if err := forge(s, c.line); !errors.As(err, &be) || !strings.Contains(be.note, c.want) {
				t.Fatalf("err %v, want a note saying %q", err, c.want)
			}
		})
	}
}

// TestHookKeyboardOnlyNeverAsked (#146 review MEDIUM-1): a keyboard-only
// command a hook reports is never asked about and never run, even at an idle
// prompt where another command would be asked about.
func TestHookKeyboardOnlyNeverAsked(t *testing.T) {
	for _, line := range []string{"/aggy accept 9f2", "/aggy trust", "/aggy tool on rtk"} {
		t.Run(line, func(t *testing.T) {
			s, ran := viewSess(t, adapter.Claude, "")
			err := forge(s, line)
			var be *blockedErr
			if codeOf(err, diag.Code{}) != diag.CodeCommandKeyboard || !errors.As(err, &be) {
				t.Fatalf("err %v, want a block for a keyboard-only command", err)
			}
			if s.hookAsk != nil || s.open || len(*ran) != 0 {
				t.Fatalf("asked %v open %v ran %q", s.hookAsk != nil, s.open, *ran)
			}
		})
	}
}

// TestPromptBusyBlocks (#146 review MEDIUM-2): a request the main loop cannot
// take, because another waits or it does not answer in time, is blocked, not
// passed to the CLI.
func TestPromptBusyBlocks(t *testing.T) {
	r := Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/switch grok"}
	s, _ := viewSess(t, adapter.Claude, "")
	s.prompts = make(chan promptReq, 1)
	s.prompts <- promptReq{} // another still waits
	var be *blockedErr
	if err := s.onSwitch(r); !errors.As(err, &be) || !strings.Contains(be.note, "still waiting") {
		t.Fatalf("err %v, want a block", err)
	}
	s.prompts = make(chan promptReq, 1) // nobody answers
	if err := s.onSwitch(r); !errors.As(err, &be) || !strings.Contains(be.note, "aggy was busy") {
		t.Fatalf("err %v, want a block", err)
	}
}

// TestAnswerBlocked (#146 review MEDIUM-2): the socket answers a blocked
// command with RespBlocked and its note, so aggy hook blocks the prompt, and
// records it unless the supervisor did.
func TestAnswerBlocked(t *testing.T) {
	line := `{"type":"prompt-command","vendor":"claude","command":"/switch grok"}`
	for _, recorded := range []bool{false, true} {
		var got []diag.Rec
		be := &blockedErr{err: coded(diag.CodeCommandNotTaken, errors.New("busy")), note: "aggy did not run /switch.", recorded: recorded}
		resp := answer([]byte(line), true, nil, func(Request) error { return be }, nil, func(r diag.Rec) { got = append(got, r) })
		if !resp.OK || resp.Type != RespBlocked || resp.Reason != be.note || resp.Code != diag.CodeCommandNotTaken.String() {
			t.Fatalf("response %+v", resp)
		}
		if want := map[bool]int{false: 1, true: 0}[recorded]; len(got) != want || want == 1 && got[0].Event != diag.EvCommandBlocked {
			t.Errorf("recorded %v: records %+v", recorded, got)
		}
	}
}
