//go:build unix

package supervisor

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
)

// freshSess is a CLI just launched: no turn end yet, its SessionStart seen
// (start) or not. Its confirmed switch is checked as production checks it
// (SwitchBlocked set, so the answer comes on childChecks: yesChecked).
func freshSess(t *testing.T, v adapter.Vendor, version string, start bool) *session {
	t.Helper()
	noSettleGap(t)
	s, _ := viewSess(t, v, "")
	s.turnRan, s.inputSinceTurn = false, false
	s.o.VendorVersion = version
	s.o.SwitchBlocked = func(adapter.Vendor, string) string { return "" }
	if s.childChecks == nil {
		s.childChecks = make(chan childCheck)
	}
	if s.quit == nil {
		s.quit = make(chan struct{})
		t.Cleanup(func() { close(s.quit) })
	}
	s.input = s.send
	if start {
		s.hookEvent(hookEvent{name: "SessionStart", sessionID: childSID, source: "startup", chain: ownChain})
	}
	return s
}

// yesChecked says yes on the switch's status line, takes the launch check's
// answer as the main loop does (childChecked), and returns what was typed.
func yesChecked(t *testing.T, s *session, to adapter.Vendor) string {
	t.Helper()
	statusYes(t, s, to)
	s.toChild = newByteQueue(maxQueued) // sentTo closed it
	select {
	case r := <-s.childChecks:
		s.childChecked(r)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer from the launch check")
	}
	return sentTo(s)
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

// TestFirstTurnAsks (T6982; #147 review, second round): on Claude with its
// SessionStart seen and no turn ended, a switch the person asked for, caught
// from the keys or picked in aggy's list, asks once on the status line, in
// every state: aggy cannot tell Claude's prompt from a start-up dialog
// (folder trust, a project's MCP servers, an update, new terms) from hooks
// alone. Never "quit it yourself", never one step. The owner's run (/switch
// codex typed 5 s after launch, at 2.1.322, 2.1.326, and a later 2.1.x) is
// the first rows. The yes types /exit (through the launch check, as in
// production); Esc stays, with nothing waiting.
func TestFirstTurnAsks(t *testing.T) {
	typed := func(s *session) { typeLine(s, "/switch codex") }
	pick := func(s *session) { pickInList(s, "2") }
	for _, c := range []struct {
		name, version string
		setup         func(s *session)
		ask           func(s *session)
	}{
		{"the owner's run, 2.1.322", checkedClaude, func(*session) {}, typed},
		{"the owner's run, 2.1.326", "2.1.326", func(*session) {}, typed},
		{"the owner's run, 2.1.340", "2.1.340", func(*session) {}, typed},
		{"a list pick, nothing typed", checkedClaude, func(*session) {}, pick},
		{"text typed, then cleared", checkedClaude, func(s *session) { typeKeys(s, "abc"); s.send([]byte(kCtrlU)) }, pick},
		{"Up first", checkedClaude, func(s *session) { s.send([]byte(kUp)) }, pick},
		{"Down first", checkedClaude, func(s *session) { s.send([]byte(kDown)) }, pick},
		{"Left first", checkedClaude, func(s *session) { s.send([]byte(kLeft)) }, pick},
		{"another version", "2.2.0", func(*session) {}, pick},
		{"no version", "", func(*session) {}, pick},
		{"the alternate screen", checkedClaude, func(s *session) { s.alt.on = true }, pick},
	} {
		for _, esc := range []bool{false, true} {
			t.Run(c.name+map[bool]string{false: ", yes", true: ", Esc"}[esc], func(t *testing.T) {
				q, records := diagHome(t)
				s := freshSess(t, adapter.Claude, c.version, true)
				s.o.Diag = q
				c.setup(s)
				c.ask(s)
				if s.sw.to != adapter.Codex {
					t.Fatalf("switch %q", s.sw.to)
				}
				if got := offerQuiet(s); got != "" {
					t.Fatalf("typed %q with no question", got)
				}
				if !s.open || s.pk.confirm != adapter.Codex || s.pk.note {
					t.Fatalf("open %v confirm %q note %v: want the status line", s.open, s.pk.confirm, s.pk.note)
				}
				if line := strings.Join(s.pk.lines, " "); !strings.Contains(line, "has not finished a turn yet") {
					t.Errorf("the status line %q does not say what aggy is unsure of", line)
				}
				if refused(t, records, diag.CodeNoTurnYet) {
					t.Error("refused with no_turn_yet")
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
				if got := yesChecked(t, s, adapter.Codex); got != "/exit\r" {
					t.Fatalf("the yes typed %q", got)
				}
			})
		}
	}
}

// TestFirstTurnNoDeadEnd (#147 review, H2; second round, H1 and L3): on
// Claude after SessionStart, an Enter that started no turn (an empty prompt,
// a dialog's answer) or a message leaves a turn that may be running. A pick
// then waits, but never into a dead end: picking it again asks on the status
// line, saying an Enter went to Claude, and its yes types /exit through the
// launch check as production runs it; the person's clean quit completes it,
// as before T6982. A key to the CLI after the question stops the yes, and a
// lapse says why on the bottom row.
func TestFirstTurnNoDeadEnd(t *testing.T) {
	again := func(t *testing.T, s *session) {
		t.Helper()
		pickInList(s, "2")
		if got := offerQuiet(s); got != "" || s.open {
			t.Fatalf("typed %q open %v: a turn may be running", got, s.open)
		}
		s.lastIn = time.Now().Add(-time.Minute)
		s.lastOut.Store(time.Now().Add(-time.Minute).UnixNano())
		pickInList(s, "2")
		if !s.open || s.pk.confirm != adapter.Codex {
			t.Fatalf("open %v confirm %q noted %q: no question", s.open, s.pk.confirm, s.noted)
		}
		if line := strings.Join(s.pk.lines, " "); !strings.Contains(line, "an Enter went to claude") {
			t.Errorf("the question %q does not say an Enter went", line)
		}
	}
	t.Run("an empty Enter, picked again: the yes types /exit", func(t *testing.T) {
		s := freshSess(t, adapter.Claude, checkedClaude, true)
		s.send([]byte("\r"))
		again(t, s)
		if got := yesChecked(t, s, adapter.Codex); got != "/exit\r" {
			t.Fatalf("the yes typed %q", got)
		}
	})
	t.Run("a message, picked again: the question says so", func(t *testing.T) {
		s := freshSess(t, adapter.Claude, checkedClaude, true)
		typeLine(s, "fix the bug")
		again(t, s)
	})
	t.Run("a key after the question stops its yes, and says why", func(t *testing.T) {
		q, records := diagHome(t)
		s := freshSess(t, adapter.Claude, checkedClaude, true)
		s.o.Diag = q
		s.send([]byte("\r"))
		again(t, s)
		statusYes(t, s, adapter.Codex)
		s.toChild = newByteQueue(maxQueued)
		s.lastIn = time.Now() // a key reached the CLI since the question
		s.noted = ""
		s.childChecked(<-s.childChecks)
		if got := sentTo(s); got != "" || s.sw.typed {
			t.Fatalf("typed %q", got)
		}
		if !strings.Contains(s.noted, "may be running a turn") || !strings.Contains(s.noted, "asks again") {
			t.Fatalf("the lapse said %q", s.noted)
		}
		if len(recAttrs(t, records(), diag.EvOfferLapsed)) != 1 {
			t.Error("no lapse recorded")
		}
	})
	t.Run("the person's clean quit completes it", func(t *testing.T) {
		s := freshSess(t, adapter.Claude, checkedClaude, true)
		s.send([]byte("\r"))
		pickInList(s, "2")
		offerQuiet(s)
		if !s.exitSwitches(0) {
			t.Fatal("a clean quit did not complete the switch")
		}
	})
	t.Run("a caught /switch, then the clean quit", func(t *testing.T) {
		s := freshSess(t, adapter.Claude, checkedClaude, true)
		s.send([]byte("\r"))
		typeLine(s, "/switch codex")
		if s.sw.to != adapter.Codex {
			t.Fatalf("switch %q", s.sw.to)
		}
		offerQuiet(s)
		if !s.exitSwitches(0) {
			t.Fatal("a clean quit did not complete the switch")
		}
	})
}

// TestFirstTurnHookAgainAsks (#147 review, L4): a repeat attempt that arrives
// as a hook's report asks, whatever switch waits: here a list pick that the
// CLI's drawing held back.
func TestFirstTurnHookAgainAsks(t *testing.T) {
	s := freshSess(t, adapter.Claude, checkedClaude, true)
	s.lastOut.Store(time.Now().UnixNano())
	pickInList(s, "2")
	s.offerSwitch()
	if s.sw.to != adapter.Codex || s.open || s.sw.typed {
		t.Fatalf("setup: switch %q open %v typed %v", s.sw.to, s.open, s.sw.typed)
	}
	// The keys saw the person's Enter for this very line; the key catch did
	// not take it.
	s.enterAt, s.afterEnter, s.enterTyped, s.enterAlone = time.Now(), 0, true, true
	s.enterLineTyped = []byte("/switch codex")
	s.toChild = newByteQueue(maxQueued)
	s.lastIn = time.Now().Add(-time.Minute)
	s.lastOut.Store(time.Now().Add(-time.Minute).UnixNano())
	if err := forge(s, "/switch codex"); err != nil {
		t.Fatalf("err %v", err)
	}
	if got := sentTo(s); strings.Contains(got, "/exit") || s.sw.typed {
		t.Fatalf("typed %q in one step from a hook's report", got)
	}
	if !s.open || s.pk.confirm != adapter.Codex {
		t.Fatalf("open %v confirm %q: want the status line", s.open, s.pk.confirm)
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

// TestHandOverDropPaste (#147 review, M2): a paste whose start came in keys
// dropped for the hand-over goes nowhere to its end, even past the window,
// so its Enter never reaches the CLI bare; what follows its end is input. A
// paste with no end in time comes back as a paste.
func TestHandOverDropPaste(t *testing.T) {
	s := cmdSess(t, adapter.Codex)
	s.handFrom = time.Now()
	s.send([]byte("\x1b[200~rm -rf"))
	s.firstOut.Store(time.Now().Add(-time.Second).UnixNano()) // the window ends
	s.send([]byte(" build\r\x1b[201~x"))
	if got := sentTo(s); got != "x" {
		t.Fatalf("the CLI got %q", got)
	}

	s2 := cmdSess(t, adapter.Codex)
	s2.sw.to, s2.sw.typed = adapter.Codex, true
	s2.exitBy = make(chan time.Time)
	s2.send([]byte("\x1b[200~one"))
	s2.exitBy = nil // the exit wait is over
	s2.dropPasteAt = time.Now().Add(-pasteDropWait - time.Second)
	s2.send([]byte("two\r\x1b[201~"))
	if got := sentTo(s2); !strings.HasPrefix(got, "\x1b[200~two") {
		t.Fatalf("past the cap the CLI got %q, want the tail as a paste", got)
	}
}

// TestHandOverDropPasteCap (#147 review, second round, M1): the reviewer's
// case. A paste that starts in the hand-over and runs past pasteDropMax, its
// end not in the read that crosses the cap: the rest reaches the CLI inside a
// paste, so no newline in it is a bare Enter.
func TestHandOverDropPasteCap(t *testing.T) {
	s := cmdSess(t, adapter.Codex)
	s.handFrom = time.Now()
	s.send([]byte("\x1b[200~"))
	s.firstOut.Store(time.Now().Add(-time.Second).UnixNano()) // the window ends
	chunk := []byte(strings.Repeat("x", 64<<10))
	for range 17 {
		s.send(chunk)
	}
	s.send([]byte("rm -rf build\recho two\r"))
	got := sentTo(s)
	open := strings.LastIndex(got, "\x1b[200~")
	if i := strings.IndexByte(got, '\r'); i >= 0 && (open < 0 || open > i) {
		t.Fatalf("a bare Enter reached the CLI: %q", got[max(0, i-20):])
	}
	if !strings.Contains(got, "rm -rf build") {
		t.Fatalf("the tail was lost: %q", got[max(0, len(got)-40):])
	}
}

// TestHandOverDropPasteSplitEnd (#147 review, second round, L1): a paste's end
// marker split across reads still ends the dropping; what follows it is input.
func TestHandOverDropPasteSplitEnd(t *testing.T) {
	for _, split := range []int{1, 3, 5} {
		s := cmdSess(t, adapter.Codex)
		s.handFrom = time.Now()
		s.send([]byte("\x1b[200~abc"))
		s.firstOut.Store(time.Now().Add(-time.Second).UnixNano())
		end := "\x1b[201~"
		s.send([]byte("def" + end[:split]))
		s.send([]byte(end[split:] + "x"))
		if got := sentTo(s); got != "x" || s.dropPaste {
			t.Fatalf("split %d: the CLI got %q (still dropping %v)", split, got, s.dropPaste)
		}
	}
}

// TestHandOverDropMouse (#147 review, L5): a mouse report in the hand-over or
// after the exit command goes nowhere, as a key does.
func TestHandOverDropMouse(t *testing.T) {
	click := []byte("\x1b[<0;10;5M")
	s := cmdSess(t, adapter.OpenCode)
	s.handFrom = time.Now()
	s.pointer(click)
	if got := sentTo(s); got != "" || s.dropped != 1 {
		t.Fatalf("in the hand-over the CLI got %q (dropped %d)", got, s.dropped)
	}
	s2 := cmdSess(t, adapter.OpenCode)
	s2.sw.to, s2.sw.typed = adapter.Codex, true
	s2.exitBy = make(chan time.Time)
	s2.pointer(click)
	if got := sentTo(s2); got != "" || s2.exitDropped != 1 {
		t.Fatalf("after the exit command the CLI got %q (dropped %d)", got, s2.exitDropped)
	}
}

// TestHandOverSignals (#147 review, M1): the hand-over takes a Ctrl+C typed
// ahead (SIGINT), and gives the signals back after HandOverGrace, so a Ctrl+C
// at aggy's own question before the next launch ends aggy as before.
func TestHandOverSignals(t *testing.T) {
	h := HandOverSignals()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if n, _ := h.Caught(); n == 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the hand-over did not take the SIGINT")
		}
	}
	h.StopAfter(20 * time.Millisecond)
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, active := h.Caught(); !active {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the hand-over kept the signals past its grace")
		}
	}
	h.Stop() // again: no panic
	if HandOverGrace > 2*time.Second {
		t.Errorf("HandOverGrace %v: aggy's own questions wait that long for their Ctrl+C", HandOverGrace)
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
