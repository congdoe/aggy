//go:build unix

package supervisor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// switchRunTurned is switchRunOn with one turn of the on-screen child
// already ended, and counted: the main loop calls Options.SessionID only
// in hookEvent, so anything typed after that call is handled after it.
func switchRunTurned(t *testing.T, vendor adapter.Vendor, more ...func(*Options)) (*termSide, *fakeChild, <-chan Exit, string) {
	t.Helper()
	seen := make(chan struct{}, 1)
	more = append(more, func(o *Options) {
		id := o.SessionID
		o.SessionID = func() string {
			select {
			case seen <- struct{}{}:
			default:
			}
			return id()
		}
	})
	ts, c, done, sock := switchRunOn(t, false, vendor, more...)
	_ = request(t, sock, fmt.Sprintf(`{"type":"hook-event","vendor":%q,"event":"Stop","payload":{"session_id":%q}}`, vendor, childSID))
	select {
	case <-seen:
	case <-time.After(waitLimit):
		t.Fatal("the turn end did not reach the main loop")
	}
	return ts, c, done, sock
}

const noTurnNote = "will not type /exit into it"

// TestSwitchNoTurnYet (T6893): until a turn end of the on-screen child is
// counted, aggy never types the exit command, since a launch dialog may be
// up: it says so, and the CLI's exit then completes the switch.
func TestSwitchNoTurnYet(t *testing.T) {
	note := func(t *testing.T, ts *termSide) {
		t.Helper()
		poll(t, "the note", []byte(noTurnNote), ts.screen)
		if strings.Contains(string(ts.screen()), "-> codex. Ctrl+") {
			t.Error("a confirmation was offered")
		}
	}
	noExit := func(t *testing.T, c *fakeChild) {
		t.Helper()
		if strings.Contains(string(c.received()), "/exit") {
			t.Errorf("the CLI got %q", c.received())
		}
	}
	t.Run("quitting completes the switch", func(t *testing.T) {
		for _, v := range []adapter.Vendor{adapter.Claude, adapter.Codex} {
			to, key := adapter.Codex, "\x1c1"
			if v == adapter.Codex {
				to = adapter.Claude
			}
			ts, c, done, _ := switchRunOn(t, false, v)
			ts.typeIn(t, key)
			poll(t, "the note", []byte(noTurnNote), ts.screen)
			never(t, "the exit command", []byte("/exit"), c.received, 300*time.Millisecond)
			c.quit(0)
			wantExit(t, done, Exit{SwitchTo: to, Next: Choice{Vendor: to}})
		}
	})
	t.Run("the switch key at the note types nothing", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		confirm(t, ts) // opens the list, where Esc cancels
		poll(t, "the list", []byte("Switching to codex"), ts.screen)
		noExit(t, c)
		ts.typeIn(t, "\x1b")
		time.Sleep(50 * time.Millisecond)
		noExit(t, c)
		finish(t, c, done) // cancelled: a plain exit
	})
	t.Run("Esc at the note keeps the switch", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		ts.typeIn(t, "\x1b")
		time.Sleep(50 * time.Millisecond)
		noExit(t, c)
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
	})
	t.Run("a turn that ends brings the normal offer", func(t *testing.T) {
		ts, c, done, sock := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		ts.typeIn(t, "hello\r") // closes the note; goes to the CLI
		c.waitGot(t, []byte("hello\r"))
		_ = request(t, sock, stopLine)
		poll(t, "the confirmation", []byte("-> codex. Ctrl+"), ts.screen)
		confirm(t, ts)
		c.waitGot(t, []byte("/exit\r"))
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
	})
	t.Run("Enter is no turn: picking again shows the note again", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "\r") // may only answer a dialog
		c.waitGot(t, []byte("\r"))
		ts.typeIn(t, "\x1c1")
		never(t, "the note before a pick says no turn runs", []byte(noTurnNote), ts.screen, 200*time.Millisecond)
		ts.typeIn(t, "\x1c")
		poll(t, "the list", []byte("Switching to codex"), ts.screen)
		ts.typeIn(t, "1")
		note(t, ts)
		noExit(t, c)
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
	})
	t.Run("another session's turn end does not count", func(t *testing.T) {
		ts, c, done, sock := switchRunOn(t, false, adapter.Claude)
		_ = request(t, sock, nestedStop)
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		noExit(t, c)
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
	})
	t.Run("unsent text: the note still comes first", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "half")
		c.waitGot(t, []byte("half"))
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
	})
	t.Run("an exit before the note ends aggy", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude, func(*Options) { switchQuiet = time.Hour })
		ts.typeIn(t, "\x1c1")
		poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
		finish(t, c, done)
	})
	t.Run("a limit alone waits for its turn end", func(t *testing.T) {
		ch := make(chan LimitHit, 1)
		ts, c, done, sock := switchRunOn(t, false, adapter.Claude, limitOpts(ch, adapter.Grok))
		ch <- LimitHit{Vendor: adapter.Claude}
		never(t, "an offer", []byte("-> grok. Ctrl+"), ts.screen, 300*time.Millisecond)
		_ = request(t, sock, stopLine)
		poll(t, "the offer", []byte("-> grok. Ctrl+"), ts.screen)
		confirm(t, ts)
		c.waitGot(t, []byte("/exit\r"))
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Brief: true}})
	})
	t.Run("a Codex limit's list leads to the note", func(t *testing.T) {
		ch := make(chan LimitHit, 1)
		ts, c, done, _ := switchRunOn(t, false, adapter.Codex, limitOpts(ch, adapter.Grok))
		ch <- LimitHit{Vendor: adapter.Codex}
		poll(t, "the limit note", []byte("codex hit its limit"), ts.screen)
		confirm(t, ts) // the list, the suggested vendor preselected
		poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
		ts.typeIn(t, "\r")
		poll(t, "the note", []byte(noTurnNote), ts.screen)
		noExit(t, c)
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Brief: true}})
	})
	t.Run("an agent's ask waits for a turn end, with no note", func(t *testing.T) {
		ts, c, done, sock := switchRunOn(t, false, adapter.Claude)
		if r := request(t, sock, `{"type":"switch-request","vendor":"codex"}`); !strings.Contains(r, `"ok":true`) {
			t.Fatalf("reply %q", r)
		}
		never(t, "a note", []byte(noTurnNote), ts.screen, 300*time.Millisecond)
		noExit(t, c)
		finish(t, c, done) // the person chose nothing: a plain exit
	})
}
