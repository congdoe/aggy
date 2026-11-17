//go:build unix

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/screen"
)

// recordedUpTo is the recording of Claude Code 2.1.326's screen
// (testdata/fixtures/claude/screen) from its start through the mark named.
func recordedUpTo(t *testing.T, mark string) []byte {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "fixtures", "claude", "screen")
	raw, err := os.ReadFile(filepath.Join(dir, "claude.raw"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "claude.marks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Marks []struct {
			State    string
			From, To int
		}
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range m.Marks {
		if k.State == mark {
			return raw[:k.To]
		}
	}
	t.Fatalf("no mark %q", mark)
	return nil
}

// withScreen gives s a copy of a 30 by 100 screen read by Claude's classifier
// at the session's version, as internal/cli does.
func withScreen(s *session) {
	s.scr = newScreenFeed(30, 100)
	version := s.o.VendorVersion
	s.o.ReadScreen = func(p *screen.Snapshot) adapter.ScreenState { return claude.ClassifyScreen(version, p) }
}

// shows feeds b to s's screen copy as copyOut and the feeder would, and
// marks the output as just drawn.
func shows(s *session, b []byte) {
	s.scr.feed(b)
	for len(s.scr.ch) > 0 {
		s.scr.apply(<-s.scr.ch)
	}
	s.lastOut.Store(time.Now().UnixNano())
}

// afterOffer runs the main loop's offer once the CLI went quiet, then takes
// the launch check's answer if one was asked, and returns what was typed
// into the CLI in all.
func afterOffer(t *testing.T, s *session) string {
	t.Helper()
	got := offerQuiet(s)
	if s.sw.checkSeq == 0 {
		return got
	}
	s.toChild = newByteQueue(maxQueued)
	select {
	case r := <-s.childChecks:
		s.childChecked(r)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer from the launch check")
	}
	return got + sentTo(s)
}

// TestFirstTurnScreen (T7016): on Claude before its first turn end, a switch
// the person asked for goes in one step, with no status-line question, only
// when the screen reads as the empty prompt and every other check holds. The
// owner's replay is the first row: a fresh Claude, /switch codex typed, and
// /exit typed with no question. Every other row asks on the status line (or,
// with a hold, says so), and types nothing.
func TestFirstTurnScreen(t *testing.T) {
	typed := func(s *session) { typeLine(s, "/switch codex") }
	pick := func(s *session) { pickInList(s, "2") }
	for _, c := range []struct {
		name, version, mark string
		before              func(s *session)
		ask                 func(s *session)
		after               func(s *session)
		oneStep             bool
	}{
		{"the owner's replay", "2.1.326", "cleared", nil, typed, nil, true},
		{"a list pick", "2.1.326", "start_idle", nil, pick, nil, true},
		{"a later patch", "2.1.340", "start_idle", nil, pick, nil, true},
		{"another view: the /model dialog", "2.1.326", "model_dialog", nil, pick, nil, false},
		{"text in the composer", "2.1.326", "typed_text", nil, pick, nil, false},
		{"a turn running", "2.1.326", "turn_running", nil, pick, nil, false},
		{"the slash menu", "2.1.326", "slash_menu", nil, pick, nil, false},
		{"Unsure", "2.1.326", "start_idle", nil, pick, func(s *session) { shows(s, []byte("\x1b7\x1b[2;5r\x1b8")) }, false},
		{"an uncovered version", "2.1.322", "start_idle", nil, pick, nil, false},
		{"no version", "", "start_idle", nil, pick, nil, false},
		{"the alternate screen", "2.1.326", "start_idle", nil, pick, func(s *session) { shows(s, []byte("\x1b[?1049h")) }, false},
		{"the alternate screen, as the tracker sees it", "2.1.326", "start_idle", nil, pick, func(s *session) { s.alt.on = true }, false},
		{"an Enter went", "2.1.326", "start_idle", func(s *session) { s.enterBeforeTurn = true }, pick, nil, false},
		{"a hold", "2.1.326", "start_idle", nil, pick, func(s *session) { s.held = true }, false},
		{"a hook's report", "2.1.326", "start_idle", nil, pick, func(s *session) { s.sw.hooked = true }, false},
		{"in doubt", "2.1.326", "start_idle", func(s *session) { s.send([]byte(kLeft)) }, pick, nil, false},
		{"the copy lags", "2.1.326", "start_idle", nil, pick, func(s *session) { s.scr.put(screenItem{data: []byte("x")}) }, false},
		{"a lost chunk", "2.1.326", "start_idle", nil, pick, func(s *session) { s.scr.gap = true }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s := freshSess(t, adapter.Claude, c.version, true)
			s.o.Diag = q
			withScreen(s)
			shows(s, recordedUpTo(t, c.mark))
			if c.before != nil {
				c.before(s)
			}
			c.ask(s)
			if c.after != nil {
				c.after(s)
			}
			got := afterOffer(t, s)
			if c.oneStep {
				if got != "/exit\r" || !s.sw.typed || s.open {
					t.Fatalf("typed %q (typed %v, open %v), want /exit in one step", got, s.sw.typed, s.open)
				}
				for _, ev := range []diag.Event{diag.EvSwitchOffered, diag.EvSwitchConfirmed} {
					a := recAttrs(t, records(), ev)
					if len(a) != 1 || a[0]["evidence"] != evidenceScreen {
						t.Errorf("%s records %v, want evidence screen", ev, a)
					}
				}
				return
			}
			if got != "" || s.sw.typed {
				t.Fatalf("typed %q with no question", got)
			}
			if s.held {
				return // a hold before the first turn end offers nothing yet
			}
			if s.sw.to != adapter.Codex {
				t.Fatalf("switch %q", s.sw.to)
			}
			if c.before != nil && s.enterBeforeTurn {
				pickInList(s, "2") // after an Enter, picking again asks
			}
			if !s.open || s.pk.confirm != adapter.Codex {
				t.Fatalf("open %v confirm %q: want the status line", s.open, s.pk.confirm)
			}
			if a := recAttrs(t, records(), diag.EvSwitchOffered); len(a) == 0 || a[len(a)-1]["evidence"] != evidencePerson {
				t.Errorf("offered records %v, want evidence person", a)
			}
		})
	}
}

// TestFirstTurnScreenHookAgain (T7016): a repeat that came from a hook's
// report never goes in one step, whatever the screen shows.
func TestFirstTurnScreenHookAgain(t *testing.T) {
	s := freshSess(t, adapter.Claude, "2.1.326", true)
	withScreen(s)
	shows(s, recordedUpTo(t, "start_idle"))
	s.setSwitch(pendingSwitch{to: adapter.Codex, picked: true, choice: Choice{Vendor: adapter.Codex}})
	s.toChild = newByteQueue(maxQueued)
	s.lastOut.Store(time.Now().Add(-time.Minute).UnixNano())
	s.offerNow(true)
	if got := sentTo(s); got != "" || s.sw.checkSeq != 0 || s.sw.typed {
		t.Fatalf("typed %q (check %d)", got, s.sw.checkSeq)
	}
	if !s.open || s.pk.confirm != adapter.Codex {
		t.Fatalf("open %v confirm %q: want the status line", s.open, s.pk.confirm)
	}
}

// TestFirstTurnScreenChanges (T7016): the screen read as the empty prompt,
// and the switch went on, but before the launch check's answer came the
// screen, a key, or the hooks changed: aggy types nothing, records why, and
// the status line asks at the next quiet moment.
func TestFirstTurnScreenChanges(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, s *session)
	}{
		{"a dialog drawn", func(t *testing.T, s *session) {
			shows(s, recordedUpTo(t, "model_dialog")[len(recordedUpTo(t, "bash_mode_left")):])
		}},
		{"the screen alone changes", func(t *testing.T, s *session) {
			out := s.lastOut.Load()
			shows(s, []byte("\x1b[?25l"))
			s.lastOut.Store(out)
		}},
		{"a key went", func(t *testing.T, s *session) { s.lastIn = time.Now() }},
		{"a hold", func(t *testing.T, s *session) {
			s.hookEvent(hookEvent{name: "Notification", sessionID: childSID, chain: ownChain})
		}},
		{"an Enter", func(t *testing.T, s *session) { s.enterBeforeTurn = true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s := freshSess(t, adapter.Claude, "2.1.326", true)
			s.o.Diag = q
			withScreen(s)
			shows(s, recordedUpTo(t, "bash_mode_left"))
			typeLine(s, "/switch codex")
			if got := offerQuiet(s); got != "" || s.sw.checkSeq == 0 || s.open {
				t.Fatalf("typed %q, check %d, open %v: want the launch check under way", got, s.sw.checkSeq, s.open)
			}
			c.change(t, s)
			s.toChild = newByteQueue(maxQueued)
			select {
			case r := <-s.childChecks:
				s.childChecked(r)
			case <-time.After(5 * time.Second):
				t.Fatal("no answer from the launch check")
			}
			if got := sentTo(s); got != "" || s.sw.typed {
				t.Fatalf("typed %q", got)
			}
			if s.held {
				return // the launch check's answer finds no prompt: it lapses, as before
			}
			if !refused(t, records, diag.CodeScreenChanged) {
				t.Error("no screen_changed record")
			}
			if s.enterBeforeTurn {
				return // the keys keep it waiting as before
			}
			s.lastIn = time.Time{}
			if got := offerQuiet(s); got != "" || !s.open || s.pk.confirm != adapter.Codex {
				t.Fatalf("typed %q, open %v confirm %q: want the status line", got, s.open, s.pk.confirm)
			}
		})
	}
}

// TestVouchedRefusalNote (#147 review follow-up): a yes the person gave
// before the first turn end, with an Enter gone to Claude (vouched), that
// typeExit then refuses after the launch check (a hold came meanwhile) says
// on the bottom row what the switch waits for, and the switch key asks again.
func TestVouchedRefusalNote(t *testing.T) {
	s := freshSess(t, adapter.Claude, "2.1.326", true)
	typeLine(s, "hello")
	pickInList(s, "2")
	pickInList(s, "2")
	if !s.open || !s.sw.vouched {
		t.Fatalf("open %v vouched %v: want the vouched status line", s.open, s.sw.vouched)
	}
	s.toChild = newByteQueue(maxQueued)
	pastGuard(s)
	s.pickerInput([]byte{0x1c})
	s.held = true // a hook's hold arrives before the launch check answers
	select {
	case r := <-s.childChecks:
		s.childChecked(r)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer from the launch check")
	}
	if got := sentTo(s); got != "" || s.sw.typed {
		t.Fatalf("typed %q", got)
	}
	if !strings.Contains(s.noted, "waits for") || !s.sw.lapsed {
		t.Errorf("bottom row %q, lapsed %v", s.noted, s.sw.lapsed)
	}
}

// TestDropPasteCapSplitEnd (#147 review follow-up): a paste dropped past its
// cap, whose end marker is split across the read before and this one, reaches
// the CLI reopened and closed: the end goes whole.
func TestDropPasteCapSplitEnd(t *testing.T) {
	for _, split := range []int{1, 3, 5} {
		s := cmdSess(t, adapter.Codex)
		s.sw.to, s.sw.typed = adapter.Codex, true
		s.exitBy = make(chan time.Time)
		end := "\x1b[201~"
		s.send([]byte("\x1b[200~one" + end[:split]))
		s.exitBy = nil
		s.dropPasteAt = time.Now().Add(-pasteDropWait - time.Second)
		s.send([]byte(end[split:] + "x"))
		got := sentTo(s)
		if !strings.HasPrefix(got, "\x1b[200~") || !strings.Contains(got, end) || s.dropPaste {
			t.Fatalf("split %d: the CLI got %q", split, got)
		}
	}
}
