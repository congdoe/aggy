//go:build unix

package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// TestBlockedRecovers (#146 review, second round, HIGH): a prompt aggy blocked
// starts no turn, so no turn end comes to settle the tracker. Before, aggy kept
// the CLI as busy and the composer in doubt, so every command after it was
// blocked too, with advice that did not work. Now a question the keys spelled
// leaves the prompt empty however it closes, and a block the keys did not
// spell is released once the CLI stays quiet, where a live check shows a
// running turn never quiet (Claude 2.1.322). After either, /switch grok typed
// in full is caught from the keys.
func TestBlockedRecovers(t *testing.T) {
	for _, c := range []struct {
		name    string
		version string
		keys    []string
		answer  func(s *session) // closes the question, if one opened
		release bool             // the quiet release applies
		caught  bool
	}{
		// The person's / drops the question and goes to the CLI.
		{"spelled question, typed over", "", []string{kBS}, func(s *session) { s.pickerInput([]byte("/")) }, false, true},
		{"spelled question, Esc", "", []string{kBS}, func(s *session) { s.pickerInput([]byte{esc}); s.promptEsc() }, false, true},
		{"Up, quiet", checkedClaude, []string{kUp}, nil, true, true},
		{"Left, Esc, quiet", checkedClaude, []string{kLeft, kEsc}, nil, true, true},
		{"Up, another version", "2.1.330", []string{kUp}, nil, false, false},
		{"Left, no Esc", checkedClaude, []string{kLeft}, nil, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.VendorVersion = c.version
			s.input = s.send
			for _, k := range c.keys {
				s.send([]byte(k))
			}
			typeLine(s, "/switch grok")
			err := forge(s, "/switch grok")
			var be *blockedErr
			if !errors.Is(err, errAsked) && !errors.As(err, &be) {
				t.Fatalf("err %v: the prompt was not blocked", err)
			}
			if c.answer != nil {
				c.answer(s)
			}
			if armed := !s.relAt.IsZero(); armed != c.release {
				t.Fatalf("release armed %v, want %v", armed, c.release)
			}
			if c.release {
				s.relAt = s.relAt.Add(-blockQuiet - time.Second) // quiet since
				s.lastIn = s.relAt.Add(-time.Second)
				s.releaseBlock()
			}
			s.toChild = newByteQueue(maxQueued)
			if s.lastByte == '/' {
				typeLine(s, "switch grok") // its / went already
			} else {
				typeLine(s, "/switch grok")
			}
			if got := s.sw.to == adapter.Grok; got != c.caught {
				t.Fatalf("caught %v, want %v (doubts %v, atPrompt %v)", got, c.caught, s.lineDoubts(), s.atPrompt)
			}
		})
	}
}

// TestReleaseNeedsQuiet (#146 review): the release waits for the CLI's output
// to stay quiet, as a running turn's does not; a key, a hold, or an ordinary
// line's Enter (not / or $) gives no release at all, so a forged request
// cannot mark a busy CLI or one at an approval idle.
func TestReleaseNeedsQuiet(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	s.o.VendorVersion = checkedClaude
	s.send([]byte(kUp))
	typeLine(s, "/switch grok")
	if _, ok := forge(s, "/switch grok").(*blockedErr); !ok || s.relAt.IsZero() {
		t.Fatal("setup: no release armed")
	}
	s.relAt = s.relAt.Add(-time.Minute / 4)
	s.lastIn = s.relAt.Add(-time.Second)
	s.lastOut.Store(time.Now().UnixNano()) // the CLI is drawing
	s.releaseBlock()
	if s.atPrompt || s.relAt.IsZero() {
		t.Fatalf("released while the CLI drew: atPrompt %v armed %v", s.atPrompt, !s.relAt.IsZero())
	}
	s.held = true // a question of the CLI's came
	s.releaseBlock()
	if s.atPrompt || !s.relAt.IsZero() {
		t.Fatal("a hold did not end the wait")
	}

	for _, c := range []struct {
		name  string
		setup func(s *session)
	}{
		{"after a message", func(s *session) { typeLine(s, "fix the bug") }},
		{"at an approval", func(s *session) { s.held = true; s.send([]byte("\r")) }},
		{"a key since the Enter", func(s *session) { typeLine(s, "/review"); s.send([]byte("x")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.VendorVersion = checkedClaude
			c.setup(s)
			_ = forge(s, "/switch grok")
			if !s.relAt.IsZero() {
				t.Fatal("a release was armed")
			}
		})
	}
}

// TestBlockNoteAdvice (#146 review): a block's note gives advice that works: the
// switch key for a switch; for another command, typing it again after a
// moment where aggy recovers, else sending a message first.
func TestBlockNoteAdvice(t *testing.T) {
	for _, c := range []struct {
		name, version, line, want string
	}{
		{"a switch", "2.1.330", "/switch grok", `Ctrl+\ opens the switch list`},
		{"recovers", checkedClaude, "/inbox", "Wait a moment, then type it in full"},
		{"does not recover", "2.1.330", "/inbox", "Send claude any message first"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.VendorVersion = c.version
			s.send([]byte(kUp))
			typeLine(s, c.line)
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
