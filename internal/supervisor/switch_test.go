//go:build unix

package supervisor

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// switchRun starts Run with a socket, claude on screen, a short quiet
// gap, and the test process as the CLI, so the test's own requests are
// FromSession. It returns the socket's path too.
func switchRun(t *testing.T, startsTurn bool) (*termSide, *fakeChild, <-chan Exit, string) {
	t.Helper()
	oldQ, oldW := switchQuiet, exitWait
	switchQuiet, exitWait = 50*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { switchQuiet, exitWait = oldQ, oldW })
	run := filepath.Join(shortDir(t), "run")
	ts, c, done := startRunOpts(t, 20*time.Millisecond, nil, func(o *Options) {
		o.RunDir, o.ParentID, o.StartsTurn = run, pid, startsTurn
	})
	return ts, c, done, filepath.Join(run, pid+".sock")
}

const stopLine = `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{}}`

// never fails if get ever shows want within d.
func never(t *testing.T, what string, want []byte, get func() []byte, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if bytes.Contains(get(), want) {
			t.Fatalf("%s: %q showed up", what, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func wantExit(t *testing.T, done <-chan Exit, want Exit) {
	t.Helper()
	select {
	case ex := <-done:
		if ex != want {
			t.Errorf("exit %+v, want %+v", ex, want)
		}
	case <-time.After(waitLimit):
		t.Fatal("Run did not return")
	}
}

// TestSwitchWaitsForTurnEnd (T6664): a picked switch waits for the turn
// end from the session, then asks, and only on Enter types /exit; the
// CLI's exit is then a switch.
func TestSwitchWaitsForTurnEnd(t *testing.T) {
	ts, c, done, sock := switchRun(t, true) // a turn is running
	ts.typeIn(t, "\x1c2")
	poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
	never(t, "mid-turn", []byte("switch to grok"), ts.screen, 300*time.Millisecond)

	if r := request(t, sock, stopLine); !strings.Contains(r, `"ok":true`) {
		t.Fatalf("hook reply %q", r)
	}
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	if !bytes.Contains(ts.screen(), []byte("not yet verified")) {
		t.Error("the confirmation does not say /exit is unverified")
	}
	if got := c.received(); len(got) != 0 {
		t.Fatalf("the CLI got %q before the user confirmed", got)
	}
	ts.typeIn(t, "\r")
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{Code: 0, SwitchTo: adapter.Grok})
}

// TestSwitchTurnEndHints: only a turn end of the vendor on screen, from
// the session, with no Enter typed since, makes the switch go on.
func TestSwitchTurnEndHints(t *testing.T) {
	tests := []struct {
		name   string
		before string // typed before the hook
		hook   string
		after  string // typed after the hook
	}{
		{"another vendor's Stop", "", `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{}}`, ""},
		{"not a turn end", "", `{"type":"hook-event","vendor":"claude","event":"Notification","payload":{}}`, ""},
		{"Enter after the Stop starts a turn", "", stopLine, "go on\r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, sock := switchRun(t, true)
			ts.typeIn(t, "\x1c2")
			poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
			_ = request(t, sock, tt.hook)
			if tt.after != "" {
				ts.typeIn(t, tt.after)
				c.waitGot(t, []byte(tt.after))
			}
			never(t, "no turn end", []byte("switch to grok"), ts.screen, 300*time.Millisecond)
			finish(t, c, done)
		})
	}
}

// TestSwitchFromOutsideSessionIgnored: a Stop that is not FromSession is
// passed on but does not count as a turn end.
func TestSwitchFromOutsideSessionIgnored(t *testing.T) {
	s := &session{o: Options{Vendor: adapter.Claude}, turnEnds: make(chan struct{}, 1)}
	s.onHook(Request{Type: ReqHookEvent, Vendor: "claude", Event: "Stop"})
	select {
	case <-s.turnEnds:
		t.Error("a Stop from outside the session counted as a turn end")
	default:
	}
	s.onHook(Request{Type: ReqHookEvent, Vendor: "claude", Event: "Stop", FromSession: true})
	select {
	case <-s.turnEnds:
	default:
		t.Error("a Stop from the session did not count")
	}
}

// TestSwitchConfirmEscStays: Esc at the confirmation types nothing and
// drops the switch.
func TestSwitchConfirmEscStays(t *testing.T) {
	ts, c, done, _ := switchRun(t, false) // at the prompt from launch
	ts.typeIn(t, "\x1c2")
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(50 * time.Millisecond) // two reads: Esc alone closes the picker
	ts.typeIn(t, "k")
	c.waitGot(t, []byte("k"))
	if got := c.received(); string(got) != "k" {
		t.Errorf("the CLI got %q", got)
	}
	finish(t, c, done)
}

// TestSwitchWarnsOfUnsentText: text typed after the last Enter is named
// in the confirmation.
func TestSwitchWarnsOfUnsentText(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "half a prompt")
	c.waitGot(t, []byte("half a prompt"))
	ts.typeIn(t, "\x1c2")
	poll(t, "the confirmation", []byte("You typed text you have not sent"), ts.screen)
	ts.typeIn(t, "\x1b")
	finish(t, c, done)
}

// TestSwitchCLIDoesNotExit: a CLI still running exitWait after /exit is
// left running, the switch dropped, and the user told.
func TestSwitchCLIDoesNotExit(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "\x1c1")
	poll(t, "the confirmation", []byte("aggy: switch to codex"), ts.screen)
	ts.typeIn(t, "\r")
	c.waitGot(t, []byte("/exit\r"))
	poll(t, "the note", []byte("did not exit"), ts.screen)
	if len(c.sigs) != 0 {
		t.Error("aggy signalled the CLI")
	}
	ts.typeIn(t, " ")
	finish(t, c, done) // no switch
}

// TestSwitchRequest (T6668): the agent's switch-request is checked and
// queued, and still waits for the turn end and the user's Enter.
func TestSwitchRequest(t *testing.T) {
	ts, c, done, sock := switchRun(t, true)
	for _, tt := range []struct{ line, want string }{
		{`{"type":"switch-request","vendor":"claude"}`, "already on screen"},
		{`{"type":"switch-request","vendor":"grok"}`, `"ok":true`},
		{`{"type":"switch-request","vendor":"codex"}`, "already waiting"},
	} {
		if r := request(t, sock, tt.line); !strings.Contains(r, tt.want) {
			t.Errorf("%s: reply %q, want %q", tt.line, r, tt.want)
		}
	}
	never(t, "mid-turn", []byte("switch to grok"), ts.screen, 200*time.Millisecond)
	_ = request(t, sock, stopLine)
	poll(t, "the confirmation", []byte("The agent asked to switch to grok."), ts.screen)
	ts.typeIn(t, "\r")
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok})
}
