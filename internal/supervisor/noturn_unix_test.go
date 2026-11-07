//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
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

const (
	noTurnNote = "will not type /exit into it"
	noTurnList = "To switch, quit claude yourself"
	// switchQuietTest is switchRunOn's quiet gap.
	switchQuietTest = 50 * time.Millisecond
)

// TestDrainSignals (T6938): a signal that arrives with the CLI's exit counts,
// though the select may take the exit first; a resize or a suspend does not.
func TestDrainSignals(t *testing.T) {
	tests := []struct {
		name string
		sigs []os.Signal
		want bool
	}{
		{"none", nil, false},
		{"a resize and a suspend", []os.Signal{syscall.SIGWINCH, syscall.SIGTSTP}, false},
		{"a terminate after a resize", []os.Signal{syscall.SIGWINCH, syscall.SIGTERM}, true},
		{"a hangup", []os.Signal{syscall.SIGHUP}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan os.Signal, 4)
			for _, sig := range tt.sigs {
				ch <- sig
			}
			s := &session{}
			s.drainSignals(ch)
			if s.signalled != tt.want || len(ch) != 0 {
				t.Errorf("signalled %v, %d left; want %v", s.signalled, len(ch), tt.want)
			}
		})
	}
}

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
	// T6938: a notice must never hold output while a turn may be running.
	// Each of these has, or may have, one, past the quiet gap, no key pressed.
	for _, tt := range []struct {
		name       string
		startsTurn bool
		before     string
		picks      int
	}{
		{"a launch with a brief", true, "", 1},
		{"after Enter", false, "\r", 1},
		{"after Enter as CSI 13 u", false, "hi\x1b[13u", 1},
		{"any input at all", false, "x", 1},
		{"a second pick after Enter says nothing of a turn", false, "\r", 2},
	} {
		t.Run("the output keeps coming: "+tt.name, func(t *testing.T) {
			ts, c, done, sock := switchRunOn(t, tt.startsTurn, adapter.Claude)
			if tt.before != "" {
				ts.typeIn(t, tt.before)
				c.waitGot(t, []byte(tt.before))
			}
			ts.typeIn(t, "\x1c1")
			poll(t, "the list's text", []byte(noTurnList), ts.screen)
			poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			if tt.picks == 2 {
				ts.clearScreen()
				ts.typeIn(t, "\x1c")
				poll(t, "the list", []byte("Switching to codex"), ts.screen)
				ts.typeIn(t, "1")
				poll(t, "the list to close", []byte(altScreenOff), ts.screen)
			}
			time.Sleep(5 * switchQuietTest)
			c.print("STREAMED")
			poll(t, "the CLI's output", []byte("STREAMED"), ts.screen)
			if strings.Contains(string(ts.screen()), noTurnNote) {
				t.Error("the note opened")
			}
			noExit(t, c)
			// The turn end still brings the offer.
			if tt.before == "" || strings.HasSuffix(tt.before, "\r") {
				_ = request(t, sock, stopLine)
				poll(t, "the confirmation", []byte("-> codex. Ctrl+"), ts.screen)
				confirm(t, ts)
				c.waitGot(t, []byte("/exit\r"))
			}
			c.quit(0)
			wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
		})
	}
	// The list said to quit, so a clean quit completes the switch, however
	// busy the output, and before any notice could show.
	t.Run("a quit right after the pick completes it", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude, func(*Options) { switchQuiet = time.Hour })
		ts.typeIn(t, "\r")
		c.waitGot(t, []byte("\r"))
		ts.typeIn(t, "\x1c1")
		poll(t, "the list to close", []byte(altScreenOff), ts.screen)
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
	})
	t.Run("a vendor no longer allowed at the quit: no switch, and it says so", func(t *testing.T) {
		var refuse atomic.Bool
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude, func(o *Options) {
			o.CanSwitch = func(adapter.Vendor) error {
				if refuse.Load() {
					return errors.New("not allowed now")
				}
				return nil
			}
		})
		ts.typeIn(t, "x")
		c.waitGot(t, []byte("x"))
		ts.typeIn(t, "\x1c1")
		poll(t, "the list to close", []byte(altScreenOff), ts.screen)
		refuse.Store(true)
		c.quit(0)
		wantExit(t, done, Exit{})
		poll(t, "why", []byte("cannot switch to codex: not allowed now"), ts.screen)
	})
	// After a turn end the list's promise is over: the exit ends aggy, saying so.
	t.Run("a pick after a turn end, then a quit, says it did not switch", func(t *testing.T) {
		ts, c, done, _ := switchRunTurned(t, adapter.Claude)
		ts.typeIn(t, "go\r")
		c.waitGot(t, []byte("go\r"))
		ts.typeIn(t, "\x1c1")
		poll(t, "the list to close", []byte(altScreenOff), ts.screen)
		c.quit(0)
		wantExit(t, done, Exit{})
		poll(t, "why", []byte("claude exited before aggy offered to switch to codex, so it did not."), ts.screen)
	})
	// A signal since the exit command was typed stops the switch too.
	t.Run("a signal after /exit was typed: no next CLI", func(t *testing.T) {
		ts, c, done, _ := switchRunTurned(t, adapter.Claude)
		ts.typeIn(t, "\x1c1")
		poll(t, "the confirmation", []byte("-> codex. Ctrl+"), ts.screen)
		confirm(t, ts)
		c.waitGot(t, []byte("/exit\r"))
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case <-c.sigs:
		case <-time.After(waitLimit):
			t.Fatal("the signal was not passed on")
		}
		c.quit(0)
		wantExit(t, done, Exit{})
		poll(t, "why", []byte("aggy was sent a signal, so aggy did not switch to codex."), ts.screen)
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
	t.Run("unsent text: a quit after the pick completes it", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "half")
		c.waitGot(t, []byte("half"))
		ts.typeIn(t, "\x1c1")
		poll(t, "the list to close", []byte(altScreenOff), ts.screen)
		c.quit(0)
		wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
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
	// T6893 review: a crash or a signal is no answer to the note.
	t.Run("a crash ends aggy with its code", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		c.quit(137)
		wantExit(t, done, Exit{Code: 137})
		poll(t, "why", []byte("claude exited with code 137, so aggy did not switch to codex."), ts.screen)
	})
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run("a forwarded "+sig.String()+" ends aggy, even at exit 0", func(t *testing.T) {
			ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
			ts.typeIn(t, "\x1c1")
			note(t, ts)
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-c.sigs:
				if got != sig {
					t.Fatalf("the CLI got %v", got)
				}
			case <-time.After(waitLimit):
				t.Fatal("the signal was not passed on")
			}
			c.quit(0)
			wantExit(t, done, Exit{})
			poll(t, "why", []byte("aggy was sent a signal, so aggy did not switch to codex."), ts.screen)
		})
	}
	// A counted turn end ends the promise: the offer takes over the note, and
	// a CLI that then exits with nothing typed ends aggy.
	t.Run("a turn end clears it: a later exit ends aggy", func(t *testing.T) {
		ts, c, done, sock := switchRunOn(t, false, adapter.Claude)
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		_ = request(t, sock, stopLine)
		poll(t, "the confirmation", []byte("-> codex. Ctrl+"), ts.screen)
		noExit(t, c)
		c.quit(0)
		wantExit(t, done, Exit{})
	})
	t.Run("the note promises no ask a CLI cannot give", func(t *testing.T) {
		ts, c, done, _ := switchRunOn(t, false, adapter.Codex)
		ts.typeIn(t, "\x1c1")
		note(t, ts)
		poll(t, "the conditional ask", []byte("some CLIs report none"), ts.screen)
		finish(t, c, done)
	})
	t.Run("a picked session that has gone: no switch, and it says so", func(t *testing.T) {
		for _, recorded := range []bool{true, false} {
			ts, c, done, _ := switchRunOn(t, false, adapter.Claude, func(o *Options) {
				o.Children = func(v adapter.Vendor) []ChildChoice {
					if v == adapter.Grok {
						return []ChildChoice{{"g2", "latest"}, {"g1", "older"}}
					}
					return nil
				}
				o.HasChild = func(_ adapter.Vendor, id string) bool { return recorded && id == "g1" }
			})
			ts.typeIn(t, "\x1c2")
			poll(t, "the child list", []byte("which session?"), ts.screen)
			ts.typeIn(t, "2")
			note(t, ts)
			c.quit(0)
			if recorded {
				wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Child: "g1"}})
				continue
			}
			wantExit(t, done, Exit{})
			poll(t, "the line", []byte("no longer recorded"), ts.screen)
		}
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
	t.Run("a signal with only an agent's ask waiting: no line", func(t *testing.T) {
		ts, c, done, sock := switchRunOn(t, false, adapter.Claude)
		if r := request(t, sock, `{"type":"switch-request","vendor":"codex"}`); !strings.Contains(r, `"ok":true`) {
			t.Fatalf("reply %q", r)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case <-c.sigs:
		case <-time.After(waitLimit):
			t.Fatal("the signal was not passed on")
		}
		c.quit(0)
		wantExit(t, done, Exit{})
		if strings.Contains(string(ts.screen()), "did not switch") {
			t.Errorf("a line for a switch nobody picked: %q", ts.screen())
		}
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
