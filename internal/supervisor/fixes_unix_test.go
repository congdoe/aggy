//go:build unix

package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// fixSess is a session on Claude whose switch to Codex CanSwitch refuses
// until wired, with SwitchFix naming aggy policy wire for that refusal.
func fixSess(t *testing.T, out string, fix bool) (*session, *[][]string, *bool) {
	t.Helper()
	s, ran := viewSess(t, adapter.Claude, out)
	noFixWindow(t)
	wired := false
	s.o.CanSwitch = func(v adapter.Vendor) error {
		if v == adapter.Codex && !wired {
			return errors.New("the safety policy cannot be applied to codex: the Codex rules file is not written")
		}
		return nil
	}
	if fix {
		s.o.SwitchFix = func(v adapter.Vendor, err error) ([]string, []string, bool) {
			if v != adapter.Codex || !strings.Contains(err.Error(), "rules file is not written") {
				return nil, nil, false
			}
			return []string{"Codex has no aggy rules file."}, []string{"policy", "wire"}, true
		}
	}
	return s, ran, &wired
}

// TestSwitchFixPage (T6979): a switch the missing Codex rules file refuses
// opens aggy's page offering aggy policy wire; y writes it and the switch goes
// on; Esc, or a fix that did not succeed, switches nowhere; with no fix named
// the refusal is said as before.
func TestSwitchFixPage(t *testing.T) {
	t.Run("y writes it, then switches", func(t *testing.T) {
		s, ran, wired := fixSess(t, "changed /h/.codex/rules/aggy.rules\nDone (run r1). aggy unwire --run r1 undoes it.", true)
		s.pickSwitch(Choice{Vendor: adapter.Codex}, true)
		g := s.pk.page
		if g == nil || strings.Join(g.confirm, " ") != "policy wire" || !strings.Contains(g.confirmLabel, "then switches to codex") || s.sw.to != "" {
			t.Fatalf("page %+v, switch %q", g, s.sw.to)
		}
		*wired = true
		s.viewInput([]byte("y"))
		settle(t, s)
		if len(*ran) != 1 || strings.Join((*ran)[0], " ") != "policy wire true" {
			t.Fatalf("ran %q", *ran)
		}
		if s.sw.to != adapter.Codex || s.pk.page != nil {
			t.Fatalf("switch %q, page %+v", s.sw.to, s.pk.page)
		}
	})
	t.Run("Esc switches nowhere", func(t *testing.T) {
		s, ran, _ := fixSess(t, "Done (run r1).", true)
		s.pickSwitch(Choice{Vendor: adapter.Codex}, true)
		s.viewInput([]byte("\x1b"))
		if s.sw.to != "" || len(*ran) != 0 {
			t.Fatalf("switch %q ran %q", s.sw.to, *ran)
		}
	})
	t.Run("a fix that failed shows its output and switches nowhere", func(t *testing.T) {
		s, _, _ := fixSess(t, "aggy policy wire: refused [wire.failed · run x]", true)
		s.pickSwitch(Choice{Vendor: adapter.Codex}, true)
		s.viewInput([]byte("y"))
		settle(t, s)
		if s.sw.to != "" || s.pk.page == nil || !strings.Contains(strings.Join(s.pk.page.lines, "\n"), "refused") {
			t.Fatalf("switch %q page %+v", s.sw.to, s.pk.page)
		}
	})
	t.Run("no fix named: refused as before", func(t *testing.T) {
		s, _, _ := fixSess(t, "", false)
		s.pickSwitch(Choice{Vendor: adapter.Codex}, true)
		if s.sw.to != "" || s.pk.page != nil && len(s.pk.page.confirm) > 0 {
			t.Fatalf("switch %q page %+v", s.sw.to, s.pk.page)
		}
	})
}

// TestHooksOffer (T6979, #133 review): an agent's switch the missing hooks
// refuse is still refused. aggy tells the person once, in a note: a y typed
// into it goes to the CLI and installs nothing; only the switch key opens the
// page, which says a new session is still needed, and y there installs them.
func TestHooksOffer(t *testing.T) {
	s, ran := viewSess(t, adapter.Claude, "Done (run r2). aggy unwire --run r2 undoes it.")
	noFixWindow(t)
	s.hookOffers = make(chan struct{}, 1)
	s.o.HooksMissing = true
	s.o.HooksFix = []string{"hooks", "install", "--vendor", "claude"}
	s.hookSeen.Store(false)
	err := s.onSwitch(Request{Vendor: "codex"})
	if err == nil || !strings.Contains(err.Error(), "the person's switch key opens aggy's page") || !strings.Contains(err.Error(), "a new session is needed") {
		t.Fatalf("refusal: %v", err)
	}
	select {
	case <-s.hookOffers:
	default:
		t.Fatal("no offer queued")
	}
	s.offerHooks()
	if s.pk.page != nil || !s.pk.note || s.pk.fix == nil || !strings.Contains(strings.Join(s.pk.lines, " "), "Press Ctrl+") {
		t.Fatalf("not a note: page %+v lines %q", s.pk.page, s.pk.lines)
	}
	// The person was typing: the y reaches the CLI, and nothing is installed.
	var typed []byte
	s.input = func(b []byte) { typed = append(typed, b...) }
	s.pickerInput([]byte("y"))
	if s.open || len(*ran) != 0 || string(typed) != "y" {
		t.Fatalf("a stray y: open %v ran %q", s.open, *ran)
	}
	s.offerHooks()
	if s.open {
		t.Fatal("told twice")
	}
	// Told again by hand, the switch key opens the page.
	s.hooksOffered = false
	s.offerHooks()
	s.pickerInput([]byte{s.pk.key})
	g := s.pk.page
	if g == nil || strings.Join(g.confirm, " ") != "hooks install --vendor claude" || !strings.Contains(strings.Join(g.lines, " "), "start a new session afterwards") {
		t.Fatalf("page %+v", g)
	}
	s.viewInput([]byte("y"))
	settle(t, s)
	if len(*ran) != 1 || strings.Join((*ran)[0], " ") != "hooks install --vendor claude true" || s.sw.to != "" {
		t.Fatalf("ran %q switch %q", *ran, s.sw.to)
	}
}

// TestFixPageTypeAhead (#133 review): a fix page ignores keys that arrive
// within checkTypeAhead of it opening, so a y typed as it appears runs
// nothing; past the window the same y runs the fix.
func TestFixPageTypeAhead(t *testing.T) {
	s, ran, _ := fixSess(t, "Done (run r1).", true)
	old := checkTypeAhead
	checkTypeAhead = time.Hour
	t.Cleanup(func() { checkTypeAhead = old })
	s.pickSwitch(Choice{Vendor: adapter.Codex}, true)
	if s.pk.page == nil {
		t.Fatal("no page")
	}
	s.viewInput([]byte("y"))
	if len(*ran) != 0 || s.pk.page == nil || s.pk.page.busy {
		t.Fatalf("a y in the window ran %q", *ran)
	}
	checkTypeAhead = 0
	s.viewInput([]byte("y"))
	settle(t, s)
	if len(*ran) != 1 {
		t.Fatalf("past the window: ran %q", *ran)
	}
}

// TestAgentSwitchFixNote (#133 review): a switch a hook sent (runCommand,
// not keyed), which the
// missing Codex rules file refuses, is told in a note rather than opened as a
// page; a stray key goes on to the CLI, and the switch key opens the page.
func TestAgentSwitchFixNote(t *testing.T) {
	s, ran, _ := fixSess(t, "Done (run r1).", true)
	noFixWindow(t)
	s.runCommand(sessioncmd.Command{Kind: sessioncmd.Switch, Vendor: adapter.Codex}, false) // from a hook
	if s.pk.page != nil || s.pk.fix == nil {
		t.Fatalf("page %+v fix %+v", s.pk.page, s.pk.fix)
	}
	var typed []byte
	s.input = func(b []byte) { typed = append(typed, b...) }
	s.pickerInput([]byte("y"))
	if s.open || len(*ran) != 0 || string(typed) != "y" {
		t.Fatalf("a stray y: open %v ran %q", s.open, *ran)
	}
	s.runCommand(sessioncmd.Command{Kind: sessioncmd.Switch, Vendor: adapter.Codex}, false) // from a hook
	s.pickerInput([]byte{s.pk.key})
	if g := s.pk.page; g == nil || strings.Join(g.confirm, " ") != "policy wire" || g.then == nil {
		t.Fatalf("page %+v", g)
	}
}

// noFixWindow turns off the fix page's type-ahead window and the note's
// guard, for a test that answers at once.
func noFixWindow(t *testing.T) {
	t.Helper()
	oldW, oldG := checkTypeAhead, confirmGuard
	checkTypeAhead, confirmGuard = 0, 0
	t.Cleanup(func() { checkTypeAhead, confirmGuard = oldW, oldG })
}
