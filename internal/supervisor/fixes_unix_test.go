//go:build unix

package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// fixSess is a session on Claude whose switch to Codex CanSwitch refuses
// until wired, with SwitchFix naming aggy policy wire for that refusal.
func fixSess(t *testing.T, out string, fix bool) (*session, *[][]string, *bool) {
	t.Helper()
	s, ran := viewSess(t, adapter.Claude, out)
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

// TestHooksOffer (T6979): an agent's switch the missing hooks refuse is still
// refused, saying the person is offered the install; aggy's page offers it
// once, says a new session is still needed, and y installs them.
func TestHooksOffer(t *testing.T) {
	s, ran := viewSess(t, adapter.Claude, "Done (run r2). aggy unwire --run r2 undoes it.")
	s.hookOffers = make(chan struct{}, 1)
	s.o.HooksMissing = true
	s.o.HooksFix = []string{"hooks", "install", "--vendor", "claude"}
	s.hookSeen.Store(false)
	err := s.onSwitch(Request{Vendor: "codex"})
	if err == nil || !strings.Contains(err.Error(), "aggy offers the person to install them") || !strings.Contains(err.Error(), "a new session is needed") {
		t.Fatalf("refusal: %v", err)
	}
	select {
	case <-s.hookOffers:
	default:
		t.Fatal("no offer queued")
	}
	s.offerHooks()
	g := s.pk.page
	if g == nil || strings.Join(g.confirm, " ") != "hooks install --vendor claude" || !strings.Contains(strings.Join(g.lines, " "), "start a new session afterwards") {
		t.Fatalf("page %+v", g)
	}
	s.viewInput([]byte("y"))
	settle(t, s)
	if len(*ran) != 1 || strings.Join((*ran)[0], " ") != "hooks install --vendor claude true" || s.sw.to != "" {
		t.Fatalf("ran %q switch %q", *ran, s.sw.to)
	}
	s.closeView()
	s.offerHooks()
	if s.pk.page != nil {
		t.Error("offered twice")
	}
}
