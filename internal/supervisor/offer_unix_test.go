//go:build unix

package supervisor

import (
	"bytes"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// TestCheckAfterYesLapses (T6982): the check after a yes that finds aggy's
// list open or a turn running types nothing, as before, but now records the
// lapse, and the switch key brings the question back.
func TestCheckAfterYesLapses(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(s *session)
		how  string
	}{
		{"the list is open", func(s *session) { s.open = true }, "picker_open"},
		{"a turn may run", func(s *session) { s.atPrompt = false }, "not_at_prompt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s := cmdSess(t, adapter.Claude)
			s.o.Diag = q
			s.setSwitch(pendingSwitch{to: adapter.Grok, picked: true, checkSeq: 1})
			c.set(s)
			s.childChecked(childCheck{seq: 1, ok: true})
			if !s.sw.lapsed || s.sw.typed {
				t.Fatalf("lapsed %v typed %v", s.sw.lapsed, s.sw.typed)
			}
			r, ok := find(records(), diag.EvOfferLapsed, diag.Code{})
			if !ok || !bytes.Contains(r.Attrs, []byte(c.how)) {
				t.Errorf("record %v %s, want how %s", ok, r.Attrs, c.how)
			}
		})
	}
}

// TestEscThenKeyLapses (#142 review L5): an Esc that turns out to start a
// longer key closes the switch's question as other keys do, and is recorded.
func TestEscThenKeyLapses(t *testing.T) {
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Claude)
	s.o.Diag = q
	s.input = s.send
	s.child = nil
	s.lastIn = time.Now().Add(-time.Minute)
	s.setSwitch(pendingSwitch{to: adapter.Grok, picked: true})
	s.offerSwitch()
	if !s.open || s.pk.confirm == "" {
		t.Fatalf("no question: open %v", s.open)
	}
	s.pickerInput([]byte{esc})
	s.pickerInput([]byte("[B"))
	if s.open || !s.sw.lapsed || s.sw.to != adapter.Grok {
		t.Fatalf("open %v lapsed %v switch %q", s.open, s.sw.lapsed, s.sw.to)
	}
	if _, ok := find(records(), diag.EvOfferLapsed, diag.Code{}); !ok {
		t.Error("no lapse recorded")
	}
}

// TestOfferKeysAfterLapse (T6982): at a switch's status line, a click changes
// nothing and the switch key still says yes; other keys close it and go to the
// CLI, and the switch key alone then brings the status line back, not the list
// (the owner's run: Ctrl+\ there opened the list, and no switch came).
func TestOfferKeysAfterLapse(t *testing.T) {
	const click = "\x1b[<0;5;5M"
	for _, c := range []struct {
		name   string
		before []string // keys at the status line before the switch key
		again  bool     // the status line closes and must come back
		lapsed bool     // a lapse is recorded
	}{
		{"the switch key", nil, false, false},
		{"a click first", []string{click}, false, false},
		{"a key first", []string{"\x7f"}, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			ts, ch, done, _ := switchRunTurned(t, adapter.Claude, func(o *Options) { o.Diag = q })
			ts.typeIn(t, "\x1c2")
			poll(t, "the status line", []byte("-> grok. Ctrl+"), ts.screen)
			for _, k := range c.before {
				time.Sleep(150 * time.Millisecond)
				ts.typeIn(t, k)
			}
			if c.again {
				ch.waitGot(t, []byte("\x7f"))
				// The CLI keeps drawing, so the quiet gap never comes and aggy
				// does not ask again on its own: only the switch key brings it.
				stop := make(chan struct{})
				drawn := make(chan struct{})
				go func() {
					defer close(drawn)
					for {
						select {
						case <-stop:
							return
						case <-time.After(10 * time.Millisecond):
							ch.print(".")
						}
					}
				}()
				mark := len(ts.screen())
				since := func() []byte { return ts.screen()[mark:] }
				never(t, "the status line on its own", []byte("-> grok. Ctrl+"), since, 200*time.Millisecond)
				ts.typeIn(t, "\x1c")
				// #142 review: not while the CLI draws; the bottom row says it
				// waits, and the question comes once the CLI is quiet.
				poll(t, "the waiting words", []byte("asks about the switch to grok once claude stops drawing"), since)
				never(t, "the status line while the CLI draws", []byte("-> grok. Ctrl+"), since, 200*time.Millisecond)
				close(stop)
				<-drawn
				poll(t, "the status line again", []byte("-> grok. Ctrl+"), since)
				if bytes.Contains(since(), []byte("aggy: switch vendor")) {
					t.Errorf("the list opened: %q", since())
				}
			}
			confirm(t, ts)
			ch.waitGot(t, []byte("/exit\r"))
			if bytes.Contains(ch.received(), []byte(click)) {
				t.Errorf("the click reached the CLI: %q", ch.received())
			}
			ch.quit(0)
			wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})
			if _, ok := find(records(), diag.EvOfferLapsed, diag.Code{}); ok != c.lapsed {
				t.Errorf("lapse recorded %v, want %v", ok, c.lapsed)
			}
		})
	}
}
