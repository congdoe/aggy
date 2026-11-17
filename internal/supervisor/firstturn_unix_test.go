//go:build unix

package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
)

// freshSess is a CLI just launched: no turn end yet, its SessionStart seen
// (start) or not.
func freshSess(t *testing.T, v adapter.Vendor, version string, start bool) *session {
	t.Helper()
	noSettleGap(t)
	s, _ := viewSess(t, v, "")
	s.turnRan, s.inputSinceTurn = false, false
	s.o.VendorVersion = version
	s.input = s.send
	if start {
		s.hookEvent(hookEvent{name: "SessionStart", sessionID: childSID, source: "startup", chain: ownChain})
	}
	return s
}

// pickInList is the switch key, then the vendor's number in the list.
func pickInList(s *session, n string) {
	s.openList()
	s.pickerInput([]byte(n))
}

// refused reports whether a record says the switch was refused with code.
func refused(t *testing.T, records func() []store.DiagRecord, code diag.Code) bool {
	t.Helper()
	for _, r := range records() {
		if r.Event == diag.EvSwitchRefused.String() && r.Code == code.String() {
			return true
		}
	}
	return false
}

// TestFirstTurnOwnerRun (T6982, the owner's run in Apple Terminal on Claude
// 2.1.322): launched, SessionStart, /switch codex typed in full 5 s later and
// caught from the keys. Before, the switch was refused (no_turn_yet) until
// the person quit Claude by hand. Now, on clean evidence, it goes in one
// step: /exit is typed once the CLI is quiet, with no question. The same at
// 2.1.326, where Claude updated itself, and later 2.1.x (the version line).
func TestFirstTurnOwnerRun(t *testing.T) {
	for _, version := range []string{checkedClaude, "2.1.326", "2.1.340"} {
		t.Run(version, func(t *testing.T) {
			q, records := diagHome(t)
			s := freshSess(t, adapter.Claude, version, true)
			s.o.Diag = q
			typeLine(s, "/switch codex")
			if s.sw.to != adapter.Codex || !s.sw.keyed || !s.sw.cleared {
				t.Fatalf("switch %q keyed %v cleared %v: not caught from the keys", s.sw.to, s.sw.keyed, s.sw.cleared)
			}
			if got := offerQuiet(s); got != "/exit\r" || !s.sw.typed || s.open {
				t.Fatalf("typed %q, typed %v, open %v: want /exit in one step", got, s.sw.typed, s.open)
			}
			if refused(t, records, diag.CodeNoTurnYet) {
				t.Error("refused with no_turn_yet")
			}
			offered := recAttrs(t, records(), diag.EvSwitchOffered)
			if len(offered) != 1 || offered[0]["first_turn"] != true {
				t.Errorf("offered records %v", offered)
			}
		})
	}
}

// TestFirstTurnListPick (T6982): a pick in aggy's own list in the same clean
// state is a deliberate key at aggy's screen, and also goes in one step.
func TestFirstTurnListPick(t *testing.T) {
	s := freshSess(t, adapter.Claude, checkedClaude, true)
	pickInList(s, "2") // codex
	if s.sw.to != adapter.Codex {
		t.Fatalf("switch %q", s.sw.to)
	}
	if got := offerQuiet(s); got != "/exit\r" {
		t.Fatalf("typed %q: want /exit in one step", got)
	}
}

// TestFirstTurnAsks (T6982): on Claude with its SessionStart seen, when any
// part of the clean evidence fails, the switch is asked about on the status
// line (one switch key), never refused with "quit it yourself". Its yes
// types /exit; Esc stays, with nothing waiting.
func TestFirstTurnAsks(t *testing.T) {
	for _, c := range []struct {
		name, version string
		setup         func(s *session)
	}{
		{"text typed, then cleared", checkedClaude, func(s *session) { typeKeys(s, "abc"); s.send([]byte(kCtrlU)) }},
		{"Up first", checkedClaude, func(s *session) { s.send([]byte(kUp)) }},
		{"Left first", checkedClaude, func(s *session) { s.send([]byte(kLeft)) }},
		{"another version", "2.2.0", func(s *session) {}},
		{"no version", "", func(s *session) {}},
		{"the alternate screen", checkedClaude, func(s *session) { s.alt.on = true }},
	} {
		for _, esc := range []bool{false, true} {
			t.Run(c.name+map[bool]string{false: ", yes", true: ", Esc"}[esc], func(t *testing.T) {
				s := freshSess(t, adapter.Claude, c.version, true)
				c.setup(s)
				pickInList(s, "2")
				if got := offerQuiet(s); got != "" {
					t.Fatalf("typed %q with no question", got)
				}
				if !s.open || s.pk.confirm != adapter.Codex || s.pk.note {
					t.Fatalf("open %v confirm %q note %v: want the status line", s.open, s.pk.confirm, s.pk.note)
				}
				if line := strings.Join(s.pk.lines, " "); !strings.Contains(line, "has not finished a turn yet") {
					t.Errorf("the status line %q does not say what aggy is unsure of", line)
				}
				if esc {
					s.toChild = newByteQueue(maxQueued)
					pastGuard(s)
					s.pickerInput([]byte{0x1b})
					s.promptEsc()
					if got := sentTo(s); got != "" || s.sw.typed {
						t.Fatalf("Esc typed %q", got)
					}
					return
				}
				if got := statusYes(t, s, adapter.Codex); got != "/exit\r" {
					t.Fatalf("the yes typed %q", got)
				}
			})
		}
	}
}

// TestFirstTurnRefused (T6982): with no SessionStart of the on-screen session
// yet, or on any CLI but Claude, a switch before the first turn end keeps
// no_turn_yet: the note says to quit the CLI by hand, and the switch key
// types nothing. Codex opened live on an update prompt whose Enter runs the
// upgrade (docs/verification.md). A hold, or output still drawing, waits.
func TestFirstTurnRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		v     adapter.Vendor
		start bool
	}{
		{"no SessionStart yet", adapter.Claude, false},
		{"Codex", adapter.Codex, true},
		{"Grok", adapter.Grok, true},
		{"OpenCode", adapter.OpenCode, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s := freshSess(t, c.v, checkedClaude, c.start)
			s.o.Diag = q
			pickInList(s, "1")
			if got := offerQuiet(s); got != "" || s.pk.confirm != "" || !s.pk.note {
				t.Fatalf("typed %q, confirm %q, note %v: want the no-turn note", got, s.pk.confirm, s.pk.note)
			}
			s.typeExit()
			if got := sentTo(s); got != "" || !refused(t, records, diag.CodeNoTurnYet) {
				t.Fatalf("typed %q; no_turn_yet recorded %v", got, refused(t, records, diag.CodeNoTurnYet))
			}
		})
	}
	t.Run("a hold waits", func(t *testing.T) {
		s := freshSess(t, adapter.Claude, checkedClaude, true)
		pickInList(s, "2")
		s.held = true
		if got := offerQuiet(s); got != "" || s.open {
			t.Fatalf("typed %q open %v under a hold", got, s.open)
		}
	})
	t.Run("output not quiet waits", func(t *testing.T) {
		s := freshSess(t, adapter.Claude, checkedClaude, true)
		pickInList(s, "2")
		s.toChild = newByteQueue(maxQueued)
		s.lastIn = time.Now().Add(-time.Minute)
		s.lastOut.Store(time.Now().UnixNano())
		s.offerSwitch()
		if got := sentTo(s); got != "" || s.open {
			t.Fatalf("typed %q open %v while the CLI draws", got, s.open)
		}
	})
}

// TestFirstTurnForged (T6982): a request from the CLI's tree never reaches
// the one-step path: before a turn end a hook's /switch opens the list only,
// and types nothing.
func TestFirstTurnForged(t *testing.T) {
	s := freshSess(t, adapter.Claude, checkedClaude, true)
	s.toChild = newByteQueue(maxQueued)
	_ = forge(s, "/switch codex")
	if s.sw.to != "" || s.sw.typed {
		t.Fatalf("switch %q typed %v from a forged request", s.sw.to, s.sw.typed)
	}
	if got := sentTo(s) + offerQuiet(s); strings.Contains(got, "/exit") {
		t.Fatalf("typed %q", got)
	}
}

// TestSwitchAgain (T6982): while a switch waits, the same switch typed in
// full again, or picked again, asks now if it can, or says on the bottom row
// what it waits for and the key that gets through; another vendor replaces
// the waiting one, and says so.
func TestSwitchAgain(t *testing.T) {
	t.Run("picked again while the CLI draws says what it waits for", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.input = s.send
		typeLine(s, "fix the bug")
		pickInList(s, "2")
		s.noted = ""
		s.lastOut.Store(time.Now().UnixNano()) // the CLI draws
		pickInList(s, "2")
		if !strings.Contains(s.noted, "waits for claude to stop drawing") {
			t.Fatalf("note %q", s.noted)
		}
	})
	t.Run("typed again at the prompt asks now", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.input = s.send
		s.send([]byte(kUp)) // a doubt: the status line asks, not one step
		pickInList(s, "2")
		if s.open {
			s.pickerInput([]byte{0x1b})
		}
		s.lastOut.Store(time.Now().Add(-time.Minute).UnixNano())
		pickInList(s, "2")
		if !s.open || s.pk.confirm != adapter.Codex && !s.pk.note {
			t.Fatalf("open %v confirm %q note %v noted %q: not asked now", s.open, s.pk.confirm, s.pk.note, s.noted)
		}
	})
	t.Run("another vendor replaces it", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.input = s.send
		typeLine(s, "fix the bug")
		pickInList(s, "2")
		pickInList(s, "1")
		if s.sw.to != adapter.Grok || !strings.Contains(s.noted, "replaces the one to codex") {
			t.Fatalf("switch %q note %q", s.sw.to, s.noted)
		}
	})
	for _, c := range []struct {
		name, line string
		to         adapter.Vendor
		note       string
	}{
		{"the same switch typed again, from the hook", "/switch codex", adapter.Codex, "waits for"},
		{"another vendor typed, from the hook", "/switch grok", adapter.Grok, "grok (not codex)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.input = s.send
			typeLine(s, "fix the bug")
			pickInList(s, "2")
			s.send([]byte(kUp)) // the key catch loses the line; the hook has it
			typeLine(s, c.line)
			before := stateOf(s)
			s.noted = ""
			if err := forge(s, c.line); err != nil {
				t.Fatalf("err %v: the person's typed switch was blocked", err)
			}
			if s.sw.to != c.to || !strings.Contains(s.noted, c.note) {
				t.Fatalf("switch %q note %q", s.sw.to, s.noted)
			}
			if got := stateOf(s); got.atPrompt != before.atPrompt || got.queued != before.queued {
				t.Fatalf("state %+v -> %+v", before, got)
			}
		})
	}
	t.Run("the hook's /switch while one waits says what it waits for", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		s.input = s.send
		typeLine(s, "fix the bug")
		pickInList(s, "2")
		var be *blockedErr
		if err := forge(s, "/switch codex"); !errors.As(err, &be) || !strings.Contains(be.note, "waits for claude's turn end") {
			t.Fatalf("err %v", err)
		}
		if s.sw.to != adapter.Codex {
			t.Fatalf("switch %q", s.sw.to)
		}
	})
}

// TestHandOverDrop (T6982): keys typed while the next CLI starts for a switch
// go nowhere until it has drawn its first screen (or handOverMax), so the
// owner's extra Ctrl+C did not end Codex; terminal reports still pass, and
// one record counts the keys.
func TestHandOverDrop(t *testing.T) {
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Codex)
	s.o.Diag = q
	s.handFrom = time.Now()
	s.send([]byte{0x03})
	s.send([]byte("\r"))
	if got := sentTo(s); got != "" {
		t.Fatalf("keys reached the new CLI: %q", got)
	}
	s.toChild = newByteQueue(maxQueued)
	s.firstOut.Store(time.Now().Add(-time.Second).UnixNano()) // it drew a while ago
	s.send([]byte("x"))
	if got := sentTo(s); got != "x" {
		t.Fatalf("after the first screen the CLI got %q", got)
	}
	d := recAttrs(t, records(), diag.EvKeysDiscarded)
	if len(d) != 1 || d[0]["count"] != float64(2) || d[0]["when"] != "start" {
		t.Errorf("records %v", d)
	}

	s2 := cmdSess(t, adapter.Codex)
	s2.handFrom = time.Now().Add(-handOverMax)
	s2.send([]byte("y"))
	if got := sentTo(s2); got != "y" {
		t.Errorf("past handOverMax the CLI got %q", got)
	}

	s3 := cmdSess(t, adapter.Claude)
	s3.sw.to, s3.sw.typed = adapter.Codex, true
	s3.exitBy = make(chan time.Time)
	s3.send([]byte{0x03})
	if got := sentTo(s3); got != "" || s3.exitDropped != 1 {
		t.Errorf("after the exit command the CLI got %q (dropped %d)", got, s3.exitDropped)
	}
}

// TestTypeExitTurnMayRun (#146 final review): the status line's yes types
// nothing when, since it opened, an Enter may have started a turn or a
// message waits queued.
func TestTypeExitTurnMayRun(t *testing.T) {
	for _, c := range []struct {
		name  string
		after func(s *session)
	}{
		{"an Enter since", func(s *session) { s.atPrompt = false }},
		{"a message queued", func(s *session) { s.queued = 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.Diag = q
			pickInList(s, "2")
			offerQuiet(s)
			if s.pk.confirm != adapter.Codex {
				t.Fatalf("setup: no status line (open %v note %v)", s.open, s.pk.confirm)
			}
			c.after(s)
			s.toChild = newByteQueue(maxQueued)
			s.typeExit()
			if got := sentTo(s); got != "" || !refused(t, records, diag.CodeTurnMayRun) {
				t.Fatalf("typed %q; turn_may_run recorded %v", got, refused(t, records, diag.CodeTurnMayRun))
			}
		})
	}
}

// TestOfferQueued (#146 final review): with a message queued the switch is not
// offered, though aggy takes the CLI as at its prompt with no doubt.
func TestOfferQueued(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	pickInList(s, "2")
	if s.open {
		s.closePicker()
	}
	s.atPrompt, s.queued = true, 1
	if got := offerQuiet(s); got != "" || s.open {
		t.Fatalf("typed %q open %v with a message queued", got, s.open)
	}
}

// TestTypedFillRefused (#146 final review): a fill the person typed, which the
// hook reports, is not typed into the CLI while a turn may run.
func TestTypedFillRefused(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	s.input = s.send
	s.send([]byte(kUp))
	typeLine(s, "/borrow i")
	s.toChild = newByteQueue(maxQueued)
	if err := forge(s, "/borrow i"); err != nil {
		t.Fatalf("err %v", err)
	}
	if !s.open || !s.pk.note || !strings.Contains(strings.Join(s.pk.lines, " "), "did not type") {
		t.Fatalf("open %v note %v lines %q", s.open, s.pk.note, s.pk.lines)
	}
	if got := sentTo(s); strings.Contains(got, "borrow") {
		t.Fatalf("typed %q", got)
	}
}
