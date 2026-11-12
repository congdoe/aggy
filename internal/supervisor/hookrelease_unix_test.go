//go:build unix

package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// cliState is what aggy believes of the CLI: where its keys go, what its
// composer holds, and whether a turn may run.
type cliState struct {
	atPrompt, lenKnown, sideView, sideEsc, input, modeLine bool
	compLen, queued                                        int
}

func stateOf(s *session) cliState {
	return cliState{s.atPrompt, s.lenKnown, s.sideView, s.sideEsc, s.inputSinceTurn, s.modeLine, s.compLen, s.queued}
}

// offerQuiet runs offerSwitch as the main loop does once input and output
// went quiet, and returns what it typed into the CLI.
func offerQuiet(s *session) string {
	s.toChild = newByteQueue(maxQueued)
	s.lastIn = time.Now().Add(-time.Minute)
	s.lastOut.Store(time.Now().Add(-time.Minute).UnixNano())
	s.offerSwitch()
	return sentTo(s)
}

// statusYes says yes on the switch's status line, and returns what that typed
// into the CLI.
func statusYes(t *testing.T, s *session, to adapter.Vendor) string {
	t.Helper()
	if !s.open || s.pk.confirm != to {
		t.Fatalf("no status line for %s: open %v confirm %q", to, s.open, s.pk.confirm)
	}
	s.toChild = newByteQueue(maxQueued)
	pastGuard(s)
	s.pickerInput([]byte{0x1c})
	return sentTo(s)
}

// TestTypedIntentNotState (#146 review, fourth round, HIGH): a command the
// person typed in full, which a prompt hook reports, is theirs (typedMatch),
// but says nothing of where its Enter went. The reviewer's cases, each with a
// forged request identical to the line: Claude's /model dialog (the line went
// into it), a /review turn and a message turn after an Esc (the line queued),
// Claude's other view after Left and Esc, and Up before the line (history
// and the line went to the model). The switch is accepted as the person's,
// but aggy's view of the CLI stays exactly as it was, and nothing is typed
// into the CLI without a yes on the status line, which waits for a turn end.
func TestTypedIntentNotState(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(s *session)
	}{
		{"Claude's /model dialog", func(s *session) { typeLine(s, "/model") }},
		{"a /review turn, the line queued", func(s *session) { typeLine(s, "/review") }},
		{"a message turn, Esc, the line queued", func(s *session) { typeLine(s, "fix the bug"); s.send([]byte(kEsc)) }},
		{"the other view after Left and Esc", func(s *session) { s.send([]byte(kLeft)); s.send([]byte(kEsc)) }},
		{"Up, then the line", func(s *session) { s.send([]byte(kUp)) }},
		{"Esc twice, then the line", func(s *session) { s.send([]byte(kEsc)); s.send([]byte(kEsc)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.VendorVersion = checkedClaude
			s.input = s.send
			c.setup(s)
			typeLine(s, "/switch grok")
			if s.sw.to != "" {
				t.Fatal("setup: the key catch took the line")
			}
			before := stateOf(s)
			s.toChild = newByteQueue(maxQueued)
			if err := forge(s, "/switch grok"); err != nil {
				t.Fatalf("err %v: the person's typed command was not accepted", err)
			}
			if s.sw.to != adapter.Grok || !s.sw.hooked || s.sw.keyed || s.sw.cleared {
				t.Fatalf("switch %q hooked %v keyed %v cleared %v, want a hook's switch", s.sw.to, s.sw.hooked, s.sw.keyed, s.sw.cleared)
			}
			if got := stateOf(s); got != before {
				t.Fatalf("state %+v -> %+v", before, got)
			}
			if got := sentTo(s) + offerQuiet(s); got != "" || s.pk.confirm != "" {
				t.Fatalf("typed %q, status line %q, with no turn end", got, s.pk.confirm)
			}
		})
	}
}

// TestTypedSpam (#146 review, fourth round): requests that do not match the
// line typed are blocked each time, and the one that does is accepted as the
// person's; none changes aggy's view of the CLI or types anything.
func TestTypedSpam(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	s.o.VendorVersion = checkedClaude
	s.input = s.send
	typeLine(s, "/review")
	typeLine(s, "/switch codex")
	before := stateOf(s)
	s.toChild = newByteQueue(maxQueued)
	for _, l := range []string{"/switch", "/switch grok", "/inbox", "/switch claude", "/switch codex"} {
		err := forge(s, l)
		if s.open && s.hookAsk != nil {
			s.closePicker()
		}
		if accepted := s.sw.to != ""; accepted != (l == "/switch codex") || accepted && err != nil {
			t.Fatalf("%s: err %v switch %q", l, err, s.sw.to)
		}
	}
	if got := stateOf(s); got != before {
		t.Fatalf("state %+v -> %+v", before, got)
	}
	if got := sentTo(s) + offerQuiet(s); got != "" {
		t.Fatalf("typed %q", got)
	}
}

// TestTypedWayThrough (#146 review, fourth round): after a stray key the
// person types the switch in full and it is accepted, and then there is
// always a way through with no model turn: a turn end the Enter started
// brings the status line, and if none comes, the switch key with the vendor
// picked again says no turn runs, and the status line asks. Its yes types
// /exit. After Left, the line typed before any Esc went to Claude's other
// view and is not taken; after the Esc it is.
func TestTypedWayThrough(t *testing.T) {
	for _, c := range []struct {
		name, version string
		keys          []string
		escAfter      bool // Left with no Esc: the first line is not taken; Esc, and type it again
		turnEnd       bool // a turn end comes; otherwise the person picks again
	}{
		{"Up, then a turn end", checkedClaude, []string{kUp}, false, true},
		{"Up, picked again", checkedClaude, []string{kUp}, false, false},
		{"Backspace, unchecked version, picked again", "", []string{kBS}, false, false},
		{"Left and Esc, picked again", checkedClaude, []string{kLeft, kEsc}, false, false},
		{"Left, then Esc, picked again", "", []string{kLeft}, true, false},
		{"Left, then Esc, a turn end", checkedClaude, []string{kLeft}, true, true},
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
			if c.escAfter {
				var be *blockedErr
				if !errors.As(err, &be) || !strings.Contains(be.note, `Ctrl+\ opens the switch list`) || s.sw.to != "" || !s.sideView {
					t.Fatalf("err %v switch %q sideView %v: a line typed into the other view was taken", err, s.sw.to, s.sideView)
				}
				s.send([]byte(kEsc))
				typeLine(s, "/switch grok")
				err = forge(s, "/switch grok")
			}
			if err != nil || s.sw.to != adapter.Grok || !s.sw.hooked {
				t.Fatalf("err %v switch %q hooked %v: the typed switch was not accepted", err, s.sw.to, s.sw.hooked)
			}
			side := s.sideView
			if got := offerQuiet(s); got != "" {
				t.Fatalf("typed %q with no turn end", got)
			}
			if c.turnEnd {
				// The line typed into the other view counted as queued: its turn
				// end comes too.
				for i := 0; i < 2 && !s.atPrompt; i++ {
					s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
				}
				if s.sideView && c.escAfter {
					t.Error("a counted turn end after the Esc left the other view")
				}
				offerQuiet(s)
			} else {
				if s.sideView != side {
					t.Error("the other view changed with no turn end")
				}
				s.switchKey()
				s.pickerInput([]byte("1")) // grok, again
				offerQuiet(s)
			}
			if got := statusYes(t, s, adapter.Grok); got != "/exit\r" {
				t.Fatalf("the yes typed %q", got)
			}
		})
	}
}

// TestTypedGuards (#146 review, fourth round): what typedMatch and the hook's
// gate refuse. Each line is /switch grok typed after a stray Up, at an idle
// prompt, so only the guard named keeps it from being accepted.
func TestTypedGuards(t *testing.T) {
	for _, c := range []struct {
		name  string
		keys  func(s *session)
		after func(s *session)
		list  bool // the list opens instead (no turn end yet)
	}{
		{"the hook too late", func(s *session) { typeLine(s, "/switch grok") }, func(s *session) { s.enterAt = s.enterAt.Add(-promptBlockWait - time.Second) }, false},
		{"a hold since", func(s *session) { typeLine(s, "/switch grok") }, func(s *session) { s.held = true }, false},
		{"an edit in the line", func(s *session) {
			typeKeys(s, "/switch grok")
			s.send([]byte{0x01}) // Ctrl+A
			s.send([]byte{0x05}) // Ctrl+E
			s.send([]byte("\r"))
		}, nil, false},
		{"a newline in the composer", func(s *session) {
			typeKeys(s, "/switch grok")
			s.lastByte = '\\' // as after a backslash: Claude's newline
			s.send([]byte("\r"))
			s.send([]byte("\r"))
		}, nil, false},
		{"before the first turn end", func(s *session) { s.turnRan = false; typeLine(s, "/switch grok") }, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.VendorVersion = checkedClaude
			s.input = s.send
			s.send([]byte(kUp))
			c.keys(s)
			if c.after != nil {
				c.after(s)
			}
			_ = forge(s, "/switch grok")
			if s.sw.to != "" || s.open != c.list || c.list && s.hookAsk != nil {
				t.Fatalf("switch %q open %v asked %v", s.sw.to, s.open, s.hookAsk != nil)
			}
		})
	}
}

// TestTypedTracker (#146 review): the line typedMatch reads: a pasted byte, a
// control key or a sequence after the first printable key spoils it; before
// it they are left out.
func TestTypedTracker(t *testing.T) {
	for _, c := range []struct {
		name  string
		steps func(s *session)
		bad   bool
	}{
		{"printable keys", func(s *session) { s.typedByte('/'); s.typedByte('a') }, false},
		{"a control key first", func(s *session) { s.typedByte(0x7f); s.typedByte('/') }, false},
		{"a sequence first", func(s *session) { s.typedSeq([]byte(kDown)); s.typedByte('/') }, false},
		{"a control key after", func(s *session) { s.typedByte('/'); s.typedByte(0x7f) }, true},
		{"a sequence after", func(s *session) { s.typedByte('/'); s.typedSeq([]byte(kLeft)) }, true},
		{"pasted", func(s *session) { s.inPaste = true; s.typedByte('/') }, true},
		{"a paste starts", func(s *session) { s.typedSeq(pasteStart) }, true},
		{"not ASCII", func(s *session) { s.typedByte(0xc3) }, true},
	} {
		s, _ := viewSess(t, adapter.Claude, "")
		c.steps(s)
		if s.typedBad != c.bad {
			t.Errorf("%s: bad %v, want %v", c.name, s.typedBad, c.bad)
		}
	}
}

// TestSpelledYesOneStep (#146 review, fourth round): the one step after a
// hook is the strict case's alone: a question about a line the keys spelled at
// the prompt, said yes to, at a version where a live check shows the blocked
// prompt leaving the composer empty.
func TestSpelledYesOneStep(t *testing.T) {
	for _, version := range []string{checkedClaude, "2.1.330"} {
		s, _ := viewSess(t, adapter.Claude, "")
		s.o.VendorVersion = version
		s.input = s.send
		s.send([]byte("\x08"))           // Ctrl+H: no live check, so the key catch loses the line
		s.send([]byte("/switch grok\r")) // its Enter in the same read: the key catch and typedMatch pass it
		c, _ := sessioncmd.Parse(adapter.Claude, "/switch grok")
		wantAsked(t, s, s.hookCommand(c, "/switch grok"), false)
		pastGuard(s)
		s.pickerInput([]byte{0x1c})
		if want := version == checkedClaude; s.sw.to != adapter.Grok || !s.sw.keyed || s.sw.cleared != want {
			t.Errorf("%s: switch %q keyed %v cleared %v, want one step %v", version, s.sw.to, s.sw.keyed, s.sw.cleared, want)
		}
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
