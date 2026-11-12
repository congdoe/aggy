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

// TestStrayKeyRunReplay (T6982, the coordinator's live run on Claude 2.1.322):
// a stray key at launch spoils the line for the key catch, and the hook's
// /switch is blocked with a note, never refused and passed to the model; a
// later turn end puts the CLI back at its prompt, and /switch codex typed in
// full is then caught from the keys, in one step. Each key the run tried is
// replayed; the ones a live check shows changing nothing on the empty composer
// are caught at once, the rest are blocked.
func TestStrayKeyRunReplay(t *testing.T) {
	for _, c := range []struct {
		name   string
		key    string
		caught bool // the first line is caught from the keys at once
	}{
		{"Backspace", kBS, true},
		{"Right", "\x1b[C", true},
		{"Home", "\x1b[H", true},
		{"End", "\x1b[F", true},
		{"Delete", "\x1b[3~", true},
		{"Ctrl+U", kCtrlU, true},
		{"Up", kUp, false},
		{"Left", kLeft, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSettleGap(t)
			q, records := diagHome(t)
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.Diag, s.o.VendorVersion = q, checkedClaude
			s.input = s.send
			s.turnRan = false // a fresh launch: no Stop yet
			s.send([]byte(c.key))
			typeLine(s, "/switch")
			if c.caught {
				if !s.open || s.pk.cmdAsk {
					t.Fatalf("the key catch did not open the switch list after %s", c.name)
				}
				s.pickerInput([]byte{esc})
				s.promptEsc()
			} else {
				if c.key == kLeft {
					// Left opened Claude's other view: the Enter went there,
					// and no prompt hook fires. A forged one changes nothing.
					if s.enterAtPrompt {
						t.Fatal("an Enter in Claude's other view counted as one at the prompt")
					}
				}
				err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/switch"})
				var be *blockedErr
				if !errors.As(err, &be) || !strings.Contains(be.note, "did not run /switch") || !strings.Contains(be.note, `Ctrl+\ opens the switch list`) {
					t.Fatalf("err %v: want the prompt blocked with a note", err)
				}
				if s.open || s.sw.to != "" {
					t.Fatal("a blocked command ran")
				}
				if len(s.blockNotes) != 1 {
					t.Errorf("bottom-row notes %d, want 1", len(s.blockNotes))
				}
				if c.key == kLeft {
					s.send([]byte{esc}) // the view's Esc
				}
			}
			// The turn the person sends next ends.
			s.toChild = newByteQueue(maxQueued)
			typeLine(s, "fix the bug")
			s.enterAt = time.Now().Add(-2 * time.Second)
			s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
			s.toChild = newByteQueue(maxQueued)
			typeLine(s, "/switch codex")
			if s.sw.to != adapter.Codex || !s.sw.keyed || !s.sw.cleared {
				t.Fatalf("switch %q keyed %v cleared %v: /switch codex typed in full was not caught in one step", s.sw.to, s.sw.keyed, s.sw.cleared)
			}
			if got := sentTo(s); got != "/switch codex\x15" {
				t.Errorf("the CLI got %q", got)
			}
			recs := records()
			for _, r := range recs {
				if r.Event == diag.EvPromptRefused.String() || r.Event == diag.EvHookForwardFailed.String() {
					t.Errorf("a hook's command was refused and passed: %s", r.Event)
				}
			}
			if b := recAttrs(t, recs, diag.EvCommandBlocked); len(b) != map[bool]int{true: 0, false: 1}[c.caught] {
				t.Errorf("blocked records %v", b)
			}
		})
	}
}

// TestHookAlwaysBlocks (T6982): every one of aggy's commands a prompt hook
// reports is blocked, whatever aggy can do about it: taken, asked about, or
// run nothing with a note. Only one turned off in the person's config goes to
// the CLI.
func TestHookAlwaysBlocks(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(s *session)
		line  string
		pass  bool
	}{
		{"a turn may run", func(s *session) { typeLine(s, "fix the bug") }, "/switch grok", false},
		{"a hold", func(s *session) { s.held = true }, "/inbox", false},
		{"unsent text", func(s *session) { typeKeys(s, "draft") }, "/inbox", false},
		{"keyboard only", func(s *session) {}, "/aggy accept 9f2", false},
		{"aggy's screen open", func(s *session) { s.openList() }, "/inbox", false},
		{"a switch waiting", func(s *session) { s.sw.to = adapter.Grok }, "/switch codex", false},
		{"before a turn end", func(s *session) { s.turnRan = false }, "/inbox", false},
		{"turned off", func(s *session) { s.o.CommandOff = func(n string) bool { return n == "inbox" } }, "/inbox", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			c.setup(s)
			err := forge(s, c.line)
			var be *blockedErr
			blocked := err == nil || errors.Is(err, errAsked) || errors.As(err, &be)
			if blocked == c.pass {
				t.Fatalf("err %v: blocked %v, want %v", err, blocked, !c.pass)
			}
		})
	}
}

// TestBlockedSpelledRecovers (T6982): a blocked prompt whose line the keys
// showed an Enter for (spelled) leaves the CLI at its prompt with its composer
// empty, so the next command typed in full is caught; a forged one, which the
// keys did not spell, changes nothing (H1).
func TestBlockedSpelledRecovers(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	s.o.VendorVersion = "2.1.330" // Backspace unchecked: the key catch loses the line
	s.askDrops = []time.Time{time.Now(), time.Now(), time.Now()}
	s.send([]byte(kBS))
	typeLine(s, "/inbox")
	if s.atPrompt {
		t.Fatal("setup: the Enter left the CLI at its prompt")
	}
	c, _ := sessioncmd.Parse(adapter.Claude, "/inbox")
	err := s.hookCommand(c, "/inbox")
	if codeOf(err, diag.Code{}) != diag.CodeCommandAskLimit {
		t.Fatalf("err %v, want the limit", err)
	}
	if !s.atPrompt || !s.lenKnown || s.mayBeRunning() {
		t.Errorf("atPrompt %v lenKnown %v: a spelled blocked prompt did not put the CLI back at its prompt", s.atPrompt, s.lenKnown)
	}

	f, _ := viewSess(t, adapter.Claude, "")
	typeLine(f, "fix the bug")
	f.askDrops = s.askDrops
	if err := forge(f, "/inbox"); codeOf(err, diag.Code{}) != diag.CodeCommandNoEnter {
		t.Fatalf("forged: err %v", err)
	}
	if f.atPrompt {
		t.Error("a forged command's block marked the CLI at its prompt during a turn")
	}
}
