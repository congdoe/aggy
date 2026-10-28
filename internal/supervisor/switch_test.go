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
	oldQ, oldW, oldG, oldE := switchQuiet, exitWait, confirmGuard, escTimeout
	switchQuiet, exitWait, confirmGuard, escTimeout = 50*time.Millisecond, 300*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { switchQuiet, exitWait, confirmGuard, escTimeout = oldQ, oldW, oldG, oldE })
	run := filepath.Join(shortDir(t), "run")
	ts, c, done := startRunOpts(t, 20*time.Millisecond, nil, func(o *Options) {
		o.RunDir, o.ParentID, o.StartsTurn = run, pid, startsTurn
		o.SessionID = func() string { return childSID }
	})
	return ts, c, done, filepath.Join(run, pid+".sock")
}

// childSID is the on-screen child's session id in switchRun.
const childSID = "11111111-2222-4333-8444-555555555555"

// stopLine is the on-screen child's Stop; nestedStop another session's.
const (
	stopLine   = `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"session_id":"` + childSID + `"}}`
	nestedStop = `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{"session_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}}`
	idlessStop = `{"type":"hook-event","vendor":"claude","event":"Stop","payload":{}}`
)

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

const notifyLine = `{"type":"hook-event","vendor":"claude","event":"Notification","payload":{}}`

// confirm presses the switch key at an open confirmation, past its guard.
func confirm(t *testing.T, ts *termSide) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	ts.typeIn(t, "\x1c")
}

// TestSwitchWaitsForTurnEnd (T6664): a picked switch waits for the turn
// end from the session, then asks, and only the switch key types /exit;
// the CLI's exit is then a switch.
func TestSwitchWaitsForTurnEnd(t *testing.T) {
	ts, c, done, sock := switchRun(t, true) // a turn is running
	ts.typeIn(t, "\x1c2")
	poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
	never(t, "mid-turn", []byte("switch to grok"), ts.screen, 300*time.Millisecond)

	if r := request(t, sock, stopLine); !strings.Contains(r, `"ok":true`) {
		t.Fatalf("hook reply %q", r)
	}
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	for _, want := range []string{"not yet verified", `Press Ctrl+\ to switch`} {
		if !bytes.Contains(ts.screen(), []byte(want)) {
			t.Errorf("the confirmation does not say %q", want)
		}
	}
	if got := c.received(); len(got) != 0 {
		t.Fatalf("the CLI got %q before the user confirmed", got)
	}
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{Code: 0, SwitchTo: adapter.Grok})
}

// TestSwitchTurnEndHints: only a turn end of the vendor on screen, from
// the session, with no Enter typed since, makes the switch go on.
func TestSwitchTurnEndHints(t *testing.T) {
	tests := []struct {
		name  string
		hook  string
		after string // typed after the hook
	}{
		{"another vendor's Stop", `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"` + childSID + `"}}`, ""},
		{"a nested CLI's Stop (S-9)", nestedStop, ""},
		{"a Stop naming no session", idlessStop, ""},
		{"not a turn end", notifyLine, ""},
		{"Enter after the Stop starts a turn", stopLine, "go on\r"},
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
// passed on but does not reach the switch.
func TestSwitchFromOutsideSessionIgnored(t *testing.T) {
	s := &session{o: Options{Vendor: adapter.Claude}, events: make(chan hookEvent, 1), quit: make(chan struct{})}
	s.onHook(Request{Type: ReqHookEvent, Vendor: "claude", Event: "Stop"})
	select {
	case <-s.events:
		t.Error("a Stop from outside the session reached the switch")
	default:
	}
	s.onHook(Request{Type: ReqHookEvent, Vendor: "claude", Event: "Stop", FromSession: true, Payload: []byte(`{"session_id":"x"}`)})
	if ev := <-s.events; ev != (hookEvent{"Stop", "x"}) {
		t.Errorf("event %+v", ev)
	}
}

// TestSwitchHolds (S-3): a Notification from the session (maybe a
// permission prompt) holds the switch through a later Stop, which a
// nested CLI can send, and through "pick again", until the user has typed
// to the CLI and a turn has ended.
func TestSwitchHolds(t *testing.T) {
	s := &session{toChild: newByteQueue(maxQueued), o: Options{SessionID: func() string { return "me" }}}
	stop := func(id string) func() { return func() { s.hookEvent(hookEvent{"Stop", id}) } }
	steps := []struct {
		do   func()
		want bool
	}{
		{stop("me"), true},
		{func() { s.hookEvent(hookEvent{"Notification", "other"}) }, false},
		{stop("me"), false}, // held
		{func() { s.hookEvent(hookEvent{"StopFailure", "me"}) }, false},
		{func() { s.send(nil); stop("other")() }, false}, // a nested CLI's Stop (S-9)
		{stop(""), false},
		{stop("me"), true}, // the user answered, and the child's turn ended
	}
	for i, st := range steps {
		st.do()
		if s.atPrompt != st.want {
			t.Errorf("step %d: atPrompt = %v, want %v", i, s.atPrompt, st.want)
		}
	}

	ts, c, done, sock := switchRun(t, true)
	ts.typeIn(t, "\x1c2")
	poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
	_ = request(t, sock, notifyLine)
	_ = request(t, sock, stopLine)
	never(t, "a held switch", []byte("aggy: switch to grok"), ts.screen, 300*time.Millisecond)
	// Picking it again cannot get past the hold.
	ts.typeIn(t, "\x1c2")
	poll(t, "the hold note", []byte("is waiting for you"), ts.screen)
	ts.typeIn(t, "\x1b")
	never(t, "a held switch", []byte("aggy: switch to grok"), ts.screen, 300*time.Millisecond)
	// The user answers (keys to the CLI), and the turn ends.
	ts.typeIn(t, "\r")
	c.waitGot(t, []byte("\r"))
	_ = request(t, sock, stopLine)
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	ts.typeIn(t, "\x1b")
	finish(t, c, done)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Error("/exit was typed")
	}
}

// TestSwitchConfirmEscStays: Esc at the confirmation types nothing and
// drops the switch.
func TestSwitchConfirmEscStays(t *testing.T) {
	ts, c, done, _ := switchRun(t, false) // at the prompt from launch
	ts.typeIn(t, "\x1c2")
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(50 * time.Millisecond) // two reads: Esc alone closes the prompt
	ts.typeIn(t, "k")
	c.waitGot(t, []byte("k"))
	if got := c.received(); string(got) != "k" {
		t.Errorf("the CLI got %q", got)
	}
	finish(t, c, done)
}

// TestSwitchBlockedByUnsentText (S-1): with text typed after the last
// Enter, aggy never offers the confirmation; neither Enter nor the switch
// key types /exit, and keys typed at the note reach the CLI.
func TestSwitchBlockedByUnsentText(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "half a prompt")
	c.waitGot(t, []byte("half a prompt"))
	ts.typeIn(t, "\x1c2")
	poll(t, "the note", []byte("you typed text you have not sent"), ts.screen)
	if bytes.Contains(ts.screen(), []byte("aggy: switch to grok")) {
		t.Fatal("a confirmation was offered over unsent text")
	}
	ts.typeIn(t, "\r")
	c.waitGot(t, []byte("half a prompt\r"))
	never(t, "unsent text", []byte("/exit"), c.received, 300*time.Millisecond)
	finish(t, c, done)
}

// TestSwitchPromptReturnsKeys (S-2): a confirmation that opens while the
// user pauses typing takes neither their keys nor their Enter: the keys
// reach the CLI as typed, and the switch key within the guard is ignored.
func TestSwitchPromptReturnsKeys(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	old := confirmGuard
	confirmGuard = time.Hour
	t.Cleanup(func() { confirmGuard = old })
	ts.typeIn(t, "\x1c2")
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	ts.typeIn(t, "\x1c") // too soon: ignored
	time.Sleep(50 * time.Millisecond)
	ts.typeIn(t, "please fix the login bug")
	time.Sleep(50 * time.Millisecond)
	ts.typeIn(t, "\r")
	c.waitGot(t, []byte("please fix the login bug\r"))
	if got := c.received(); string(got) != "please fix the login bug\r" {
		t.Errorf("the CLI got %q", got)
	}
	finish(t, c, done)
}

// TestSwitchLateExit (S-4): a CLI that exits after exitWait is still
// switched from, and the user was told it had not exited yet.
func TestSwitchLateExit(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "\x1c1")
	poll(t, "the confirmation", []byte("aggy: switch to codex"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	poll(t, "the note", []byte("has not exited"), ts.screen)
	if len(c.sigs) != 0 {
		t.Error("aggy signalled the CLI")
	}
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Codex})
}

// TestSwitchCancelAfterExitTyped (S-4): Esc in the picker after /exit was
// typed cancels the switch, so the CLI's exit ends aggy.
func TestSwitchCancelAfterExitTyped(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "\x1c1")
	poll(t, "the confirmation", []byte("aggy: switch to codex"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	poll(t, "the note", []byte("has not exited"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(50 * time.Millisecond)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("Switching to codex"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(50 * time.Millisecond)
	finish(t, c, done)
}

// TestSwitchRequest (T6668): the agent's switch-request is checked and
// queued, and still waits for the turn end and the user's switch key.
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
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok})
}

// TestSwitchRepickKeepsUnsentText (S-8): picking the waiting vendor again
// does not clear unsent text, so the switch key never sends draft+/exit.
func TestSwitchRepickKeepsUnsentText(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "half a draft")
	c.waitGot(t, []byte("half a draft"))
	ts.typeIn(t, "\x1c2")
	poll(t, "the note", []byte("you typed text you have not sent"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "\x1c2") // pick it again
	time.Sleep(600 * time.Millisecond)
	ts.typeIn(t, "\x1c")
	time.Sleep(200 * time.Millisecond)
	if got := c.received(); bytes.Contains(got, []byte("/exit")) {
		t.Fatalf("the CLI got %q", got)
	}
	if bytes.Contains(ts.screen(), []byte("aggy: switch to grok")) {
		t.Error("a confirmation was offered over unsent text")
	}
	finish(t, c, done)
}

// TestSwitchNeedsChildStop (S-9): a Stop naming another session does not
// open the confirmation; the child's own does.
func TestSwitchNeedsChildStop(t *testing.T) {
	ts, c, done, sock := switchRun(t, true)
	ts.typeIn(t, "\x1c2")
	poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
	_ = request(t, sock, nestedStop)
	never(t, "a nested Stop", []byte("aggy: switch to grok"), ts.screen, 300*time.Millisecond)
	_ = request(t, sock, stopLine)
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok})
}

// TestSwitchKeyAtNoteOpensList (S-7): at the late-exit note the switch
// key opens the list, as the note says, and neither it nor the Esc that
// cancels reaches the CLI.
func TestSwitchKeyAtNoteOpensList(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "\x1c1")
	poll(t, "the confirmation", []byte("aggy: switch to codex"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	poll(t, "the note", []byte("has not exited"), ts.screen)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("Switching to codex"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(60 * time.Millisecond)
	if got := c.received(); string(got) != "/exit\r" {
		t.Errorf("the CLI got %q", got)
	}
	finish(t, c, done) // cancelled: no switch
}

// TestSwitchSplitEscHandedBack (S-11): an Esc that starts a longer key
// at a confirmation is handed to the CLI whole and does not cancel.
func TestSwitchSplitEscHandedBack(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	old := escTimeout
	escTimeout = time.Second
	t.Cleanup(func() { escTimeout = old })
	ts.typeIn(t, "\x1c2")
	poll(t, "the confirmation", []byte("aggy: switch to grok"), ts.screen)
	ts.typeIn(t, "\x1b")
	time.Sleep(30 * time.Millisecond)
	ts.typeIn(t, "[A")
	c.waitGot(t, []byte("\x1b[A"))
	ts.typeIn(t, "\x1c") // the switch still waits: the list shows it
	poll(t, "the list", []byte("Switching to grok"), ts.screen)
	ts.typeIn(t, "\x1b")
	finish(t, c, done)
}
