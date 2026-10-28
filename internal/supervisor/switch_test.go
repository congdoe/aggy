//go:build unix

package supervisor

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/congdoe/aggy/internal/adapter"
)

// switchRun starts Run with a socket, claude on screen, a short quiet
// gap, and the test process as the CLI, so the test's own requests are
// FromSession. It returns the socket's path too.
func switchRun(t *testing.T, startsTurn bool) (*termSide, *fakeChild, <-chan Exit, string) {
	t.Helper()
	return switchRunOn(t, startsTurn, adapter.Claude)
}

// switchRunOn is switchRun with vendor on screen; the picker offers the
// other two, in claude, codex, grok order.
func switchRunOn(t *testing.T, startsTurn bool, vendor adapter.Vendor, more ...func(*Options)) (*termSide, *fakeChild, <-chan Exit, string) {
	t.Helper()
	oldQ, oldW, oldG, oldE := switchQuiet, exitWait, confirmGuard, escTimeout
	switchQuiet, exitWait, confirmGuard, escTimeout = 50*time.Millisecond, 300*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { switchQuiet, exitWait, confirmGuard, escTimeout = oldQ, oldW, oldG, oldE })
	run := filepath.Join(shortDir(t), "run")
	ts, c, done := startRunOpts(t, 20*time.Millisecond, nil, func(o *Options) {
		o.RunDir, o.ParentID, o.StartsTurn = run, pid, startsTurn
		o.SessionID = func() string { return childSID }
		o.Vendor, o.Vendors = vendor, nil
		for _, v := range adapter.Vendors() {
			if v != vendor {
				o.Vendors = append(o.Vendors, v)
			}
		}
		for _, f := range more {
			f(o)
		}
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
	for _, want := range []string{"See claude's prompt?", `Ctrl+\ types /exit`} {
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
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})
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
	wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex}})
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
	if err := pty.Setsize(ts.master, &pty.Winsize{Rows: 24, Cols: 160}); err != nil {
		t.Fatal(err)
	}
	_ = request(t, sock, stopLine)
	poll(t, "the confirmation", []byte("(The agent asked.)"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})
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
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})
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

// TestSwitchConfirmKeepsScreen (S-12): the confirmation is one status
// line on the bottom row, with the cursor saved and put back: it never
// switches screens or clears the CLI's, so a pending approval stays in
// view.
func TestSwitchConfirmKeepsScreen(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	c.print("APPROVE? 1. Yes 2. No")
	poll(t, "the CLI", []byte("APPROVE?"), ts.screen)
	ts.typeIn(t, "\x1c2")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	mark := len(ts.screen())
	poll(t, "the confirmation", []byte("aggy: switch to grok?"), ts.screen)
	time.Sleep(50 * time.Millisecond)
	drawn := ts.screen()[mark:]
	for _, bad := range []string{altScreenOn, "\x1b[?1049", "\x1b[2J", "\x1b[H"} {
		if bytes.Contains(drawn, []byte(bad)) {
			t.Errorf("the confirmation drew %q: %q", bad, drawn)
		}
	}
	if !bytes.HasPrefix(drawn, []byte("\x1b7\x1b[24;1H\x1b[2K")) || !bytes.Contains(drawn, []byte("\x1b8")) {
		t.Errorf("the confirmation is not one saved-cursor line on row 24: %q", drawn)
	}
	ts.typeIn(t, "\x1b")
	poll(t, "the line cleared", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b8"), ts.screen)
	finish(t, c, done)
}

// TestSwitchCodexAgentRefused (S-12): on Codex, which has no hold signal,
// an agent's switch-request is refused, so a forged Stop can never lead
// to /exit; a switch the user starts still works.
func TestSwitchCodexAgentRefused(t *testing.T) {
	ts, c, done, sock := switchRunOn(t, true, adapter.Codex)
	codexStop := `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"` + childSID + `"}}`
	if r := request(t, sock, `{"type":"switch-request","vendor":"grok"}`); !strings.Contains(r, "an agent cannot ask for a switch away from codex") {
		t.Errorf("switch-request reply %q", r)
	}
	_ = request(t, sock, codexStop) // forged, from the tree
	never(t, "an agent switch on codex", []byte("aggy: switch to"), ts.screen, 300*time.Millisecond)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	ts.typeIn(t, "\x1c") // the switch key closes the list; nothing typed
	time.Sleep(200 * time.Millisecond)
	if got := c.received(); bytes.Contains(got, []byte("/exit")) {
		t.Fatalf("the CLI got %q", got)
	}

	// The user picks grok themselves, and confirms at the turn end.
	ts.typeIn(t, "\x1c2")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	_ = request(t, sock, codexStop)
	poll(t, "the confirmation", []byte("aggy: switch to grok? See codex's prompt?"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})
}

// TestSendDirty (S-13): only a plain Enter sends; Claude's newline keys
// (Option+Enter, backslash then Enter, even across reads) and paste
// content leave the prompt dirty.
func TestSendDirty(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		dirty  bool
	}{
		{"typed and sent", []string{"hello", "\r"}, false},
		{"sent in one read", []string{"hello\r"}, false},
		{"typed, not sent", []string{"hello"}, true},
		{"option+enter", []string{"line one", "\x1b\r"}, true},
		{"backslash enter, one read", []string{"line one\\\r"}, true},
		{"backslash enter, two reads", []string{"line one\\", "\r"}, true},
		{"newline then more", []string{"a\x1b\r", "b"}, true},
		{"newline, then sent", []string{"a\x1b\r", "b", "\r"}, false},
		{"paste ending in Enter", []string{"\x1b[200~x\r\x1b[201~"}, true},
		{"paste open, Enter inside", []string{"\x1b[200~x\r"}, true},
		{"paste, then sent", []string{"\x1b[200~x\x1b[201~", "\r"}, false},
	}
	for _, tt := range tests {
		s := &session{toChild: newByteQueue(maxQueued)}
		for _, ch := range tt.chunks {
			s.send([]byte(ch))
		}
		if s.dirty != tt.dirty {
			t.Errorf("%s: dirty = %v, want %v", tt.name, s.dirty, tt.dirty)
		}
	}
}

// TestSwitchMultilineDraft (S-13): a draft with a newline key, then a
// re-pick and the switch key, never types /exit.
func TestSwitchMultilineDraft(t *testing.T) {
	for _, nl := range [][]string{{"\x1b\r"}, {"\\\r"}, {"\\", "\r"}} {
		ts, c, done, _ := switchRun(t, false)
		ts.typeIn(t, "line one")
		c.waitGot(t, []byte("line one"))
		for _, k := range nl {
			time.Sleep(20 * time.Millisecond)
			ts.typeIn(t, k)
		}
		c.waitGot(t, []byte(strings.Join(nl, "")))
		// The newline key holds an Enter, so the first pick waits for a
		// turn end; picking again says none runs, and the draft blocks.
		ts.typeIn(t, "\x1c2")
		poll(t, "the list to close", []byte(altScreenOff), ts.screen)
		time.Sleep(60 * time.Millisecond)
		ts.typeIn(t, "\x1c2")
		poll(t, "the note", []byte("you typed text you have not sent"), ts.screen)
		time.Sleep(400 * time.Millisecond)
		ts.typeIn(t, "\x1c")
		time.Sleep(200 * time.Millisecond)
		if got := c.received(); bytes.Contains(got, []byte("/exit")) {
			t.Fatalf("%q: the CLI got %q", nl, got)
		}
		finish(t, c, done)
	}
}

// TestSwitchOptionsCarried (T6665): the picker's toggles and a
// switch-request's options reach the caller with the switch.
func TestSwitchOptionsCarried(t *testing.T) {
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	for _, k := range []string{"b", "m", "gpt-x", "\r"} {
		ts.typeIn(t, k)
		time.Sleep(20 * time.Millisecond)
	}
	poll(t, "the toggles", []byte("brief: on    m  model for this launch: gpt-x"), ts.screen)
	ts.typeIn(t, "1")
	poll(t, "the confirmation", []byte("aggy: switch to codex?"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Codex, Next: Choice{Vendor: adapter.Codex, Model: "gpt-x", Brief: true}})

	ts, c, done, sock := switchRun(t, false)
	if r := request(t, sock, `{"type":"switch-request","vendor":"grok","new":true,"model":"g-1","brief":true}`); !strings.Contains(r, `"ok":true`) {
		t.Fatalf("reply %q", r)
	}
	for _, bad := range []string{
		`{"type":"switch-request","vendor":"grok","model":"a b"}`,
		`{"type":"hook-event","vendor":"claude","event":"Stop","payload":{},"brief":true}`,
	} {
		if r := request(t, sock, bad); strings.Contains(r, `"ok":true`) {
			t.Errorf("%s: accepted", bad)
		}
	}
	poll(t, "the confirmation", []byte("aggy: switch to grok?"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, New: true, Model: "g-1", Brief: true}})
}

// limitOpts gives Run a limit channel, and quota to grok only.
func limitOpts(ch chan adapter.Vendor, quota ...adapter.Vendor) func(*Options) {
	return func(o *Options) {
		o.Limits = ch
		o.HasQuota = func(v adapter.Vendor) bool {
			for _, q := range quota {
				if q == v {
					return true
				}
			}
			return false
		}
	}
}

// TestSwitchOnLimit (T6667): a limit of the vendor on screen queues a
// switch to the first vendor with quota, with a brief, which still waits
// for the turn end and the switch key: one key accepts, none is pressed
// for the user.
func TestSwitchOnLimit(t *testing.T) {
	ch := make(chan adapter.Vendor, 1)
	ts, c, done, sock := switchRunOn(t, true, adapter.Claude, limitOpts(ch, adapter.Grok))
	if err := pty.Setsize(ts.master, &pty.Winsize{Rows: 24, Cols: 160}); err != nil {
		t.Fatal(err)
	}
	ch <- adapter.Codex // not the vendor on screen: ignored
	ch <- adapter.Claude
	never(t, "mid-turn", []byte("aggy: switch to"), ts.screen, 200*time.Millisecond)
	_ = request(t, sock, `{"type":"hook-event","vendor":"claude","event":"StopFailure","payload":{"session_id":"`+childSID+`","error":"rate_limit"}}`)
	poll(t, "the confirmation", []byte("(claude hit its limit.)"), ts.screen)
	if !bytes.Contains(ts.screen(), []byte("aggy: switch to grok?")) {
		t.Fatal("codex has no quota; grok should be offered")
	}
	time.Sleep(300 * time.Millisecond)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Fatal("/exit typed without the switch key")
	}
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Brief: true}})
}

// TestSwitchOnLimitCodex (T6667, S-12): Codex has no hold signal, so a
// limit only says so and preselects; the user opens the list, Enter takes
// the preselected vendor with a brief, and the switch key confirms.
func TestSwitchOnLimitCodex(t *testing.T) {
	ch := make(chan adapter.Vendor, 1)
	ts, c, done, sock := switchRunOn(t, true, adapter.Codex, limitOpts(ch, adapter.Grok))
	ch <- adapter.Codex
	poll(t, "the note", []byte("codex hit its limit. grok has quota left"), ts.screen)
	_ = request(t, sock, `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"`+childSID+`"}}`)
	time.Sleep(200 * time.Millisecond)
	if bytes.Contains(c.received(), []byte("/exit")) || bytes.Contains(ts.screen(), []byte("aggy: switch to")) {
		t.Fatal("a limit on codex offered a switch by itself")
	}
	ts.typeIn(t, "\x1c") // at the note: opens the list
	poll(t, "the preselect", []byte(" >2  grok"), ts.screen)
	poll(t, "brief on", []byte("b  brief: on"), ts.screen)
	ts.typeIn(t, "\r")
	poll(t, "the confirmation", []byte("aggy: switch to grok?"), ts.screen)
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok, Brief: true}})
}

// TestSwitchOnLimitNoQuota (T6667): with no vendor left, aggy says so
// and queues nothing.
func TestSwitchOnLimitNoQuota(t *testing.T) {
	ch := make(chan adapter.Vendor, 1)
	ts, c, done, _ := switchRunOn(t, false, adapter.Claude, limitOpts(ch))
	ch <- adapter.Claude
	poll(t, "the note", []byte("no other vendor aggy offers has quota left"), ts.screen)
	ts.typeIn(t, "\x1b")
	never(t, "no switch", []byte("aggy: switch to"), ts.screen, 300*time.Millisecond)
	finish(t, c, done)
}
