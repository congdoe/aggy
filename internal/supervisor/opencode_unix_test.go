//go:build unix

package supervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/hook"
)

func TestDropReports(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain typing", "hello\r", "hello\r"},
		{"SGR mouse motion", "\x1b[<35;10;5M", ""},
		{"SGR press and release", "\x1b[<0;1;1M\x1b[<0;1;1m", ""},
		{"focus in and out", "\x1b[I\x1b[O", ""},
		{"a colour scheme notice", "\x1b[?997;1n", ""},
		{"a keyboard flags reply", "\x1b[?1u", ""},
		{"typing around a report", "a\x1b[<35;1;1Mb", "ab"},
		{"an arrow key stays", "\x1b[A", "\x1b[A"},
		{"a kitty key stays", "\x1b[92;5u", "\x1b[92;5u"},
		{"a paste marker stays", "\x1b[200~x\x1b[201~", "\x1b[200~x\x1b[201~"},
		{"a report cut short stays: when in doubt it counts", "\x1b[<35;1", "\x1b[<35;1"},
		{"a lone Esc stays", "\x1b", "\x1b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dropReports([]byte(c.in)); string(got) != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// openCodeSession is a session with a switch waiting at a quiet prompt.
func openCodeSession(t *testing.T) *session {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	return &session{o: Options{Vendor: adapter.OpenCode, Out: out, SessionID: func() string { return "ses_a" }},
		t: &tty{fd: -1}, pk: &picker{key: 0x1c}, toChild: newByteQueue(maxQueued), atPrompt: true, turnRan: true, lenKnown: true, quit: make(chan struct{}), events: make(chan hookEvent, 4)}
}

// popAll is b and whatever else is queued now, up to the exit command.
func popAll(s *session, b []byte) []byte {
	for !bytes.HasSuffix(b, []byte("/exit")) {
		next, ok := s.toChild.pop()
		if !ok {
			break
		}
		b = append(b, next...)
	}
	return b
}

func sentTo(s *session) string {
	s.toChild.close()
	var got []byte
	for b, ok := s.toChild.pop(); ok; b, ok = s.toChild.pop() {
		got = append(got, b...)
	}
	return string(got)
}

// TestOpenCodeExitWaitsForAReply: over a permission dialog, the exit
// command's Enter chooses Allow once (fake model, T6930), so aggy types
// nothing while one is open, whether or not the user has typed since.
func TestOpenCodeExitWaitsForAReply(t *testing.T) {
	for _, c := range []struct {
		name         string
		asked, reply string
	}{
		{"a permission", hook.EventPermissionAsked, hook.EventPermissionReplied},
		{"a question", hook.EventQuestionAsked, hook.EventQuestionReplied},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openCodeSession(t)
			s.setSwitch(pendingSwitch{to: adapter.Claude})
			s.hookEvent(hookEvent{name: c.asked, sessionID: "ses_a", request: "r1"})
			if s.atPrompt || !s.held || !s.askOpen() {
				t.Fatalf("after asked: atPrompt %v, held %v, asked %v", s.atPrompt, s.held, s.asked)
			}
			// Keys that move in the dialog clear held, as for any vendor. The
			// dialog is still open.
			s.send([]byte("\x1b[B"))
			if s.held {
				t.Fatal("typing should clear held")
			}
			s.atPrompt = true
			s.typeExit()
			if s.sw.typed {
				t.Fatal("the exit command was typed over an open dialog")
			}
			// A second ask stays open after the first's reply.
			s.hookEvent(hookEvent{name: c.asked, sessionID: "ses_a", request: "r2"})
			s.hookEvent(hookEvent{name: c.reply, sessionID: "ses_a", request: "r1"})
			if !s.askOpen() || !s.held {
				t.Fatalf("one of two replied: asked %v, held %v", s.asked, s.held)
			}
			s.hookEvent(hookEvent{name: c.reply, sessionID: "ses_a", request: "r2"})
			if s.askOpen() || s.held {
				t.Fatalf("all replied: asked %v, held %v", s.asked, s.held)
			}
			// The reply alone is no turn end.
			if s.atPrompt = false; s.atPrompt {
				t.Fatal("unreachable")
			}
			s.hookEvent(hookEvent{name: "Stop", sessionID: "ses_a"})
			if !s.atPrompt {
				t.Fatal("a turn end after the reply should put it at its prompt")
			}
			s.typeExit()
			if !s.sw.typed {
				t.Fatal("the exit command should now be typed")
			}
			if b, ok := s.toChild.pop(); !ok || !bytes.HasSuffix(popAll(s, b), []byte("/exit")) {
				t.Fatal("no exit command was typed")
			}
			if !s.exitGap {
				t.Fatal("the Enter should wait for the gap")
			}
		})
	}
}

// gapSession has typed the exit command and is in the gap.
func gapSession(t *testing.T) *session {
	t.Helper()
	old := exitEnterGap
	t.Cleanup(func() { exitEnterGap = old })
	exitEnterGap = time.Millisecond
	s := openCodeSession(t)
	s.setSwitch(pendingSwitch{to: adapter.Claude})
	t.Cleanup(s.closePicker)
	s.typeExit()
	if !s.sw.typed {
		t.Fatal("exit not typed")
	}
	if b, ok := s.toChild.pop(); !ok || string(b) != "/exit" {
		t.Fatalf("first = %q", b)
	}
	if ev := <-s.events; ev.name != exitEnterCheck {
		t.Fatalf("event = %+v", ev)
	}
	return s
}

// TestExitEnterGap: on the full screen an Enter in the same burst as the
// letters was lost (live), so it comes on its own, later.
func TestExitEnterGap(t *testing.T) {
	s := gapSession(t)
	s.hookEvent(hookEvent{name: exitEnterCheck})
	if got := sentTo(s); got != "\r" {
		t.Fatalf("sent %q", got)
	}
}

// TestDialogInGap (H4): a dialog reported in the gap gets no Enter, which
// would choose Allow once, and the person is told about the text left behind.
func TestDialogInGap(t *testing.T) {
	s := gapSession(t)
	s.hookEvent(hookEvent{name: hook.EventPermissionAsked, sessionID: "ses_a", request: "r1"})
	s.send([]byte("x"))
	s.hookEvent(hookEvent{name: exitEnterCheck})
	if s.sw.to != "" || s.sw.typed || s.exitBy != nil {
		t.Errorf("the switch stands: %+v", s.sw)
	}
	if !s.dirty {
		t.Error("the letters must count as unsent text")
	}
	if !s.open || !strings.Contains(strings.Join(s.pk.lines, " "), "/exit may be in its prompt") {
		t.Errorf("not told: open %v, %q", s.open, s.pk.lines)
	}
	if got := sentTo(s); got != "x" {
		t.Errorf("sent %q, want only the key, which goes to the dialog", got)
	}
}

// TestTypingInGap (M5): keys in the gap never land between /exit and its
// Enter. The command is taken back, and they reach the prompt, unsent.
func TestTypingInGap(t *testing.T) {
	s := gapSession(t)
	s.send([]byte("hello"))
	if s.compLen != 0 || s.dirty {
		t.Errorf("held keys counted early: %d, %v", s.compLen, s.dirty)
	}
	s.hookEvent(hookEvent{name: exitEnterCheck})
	if s.sw.to != "" || s.exitBy != nil {
		t.Errorf("the switch stands: %+v", s.sw)
	}
	if !s.dirty || s.compLen != len("hello") {
		t.Errorf("dirty %v, compLen %d", s.dirty, s.compLen)
	}
	if got := sentTo(s); got != "\x7f\x7f\x7f\x7f\x7fhello" {
		t.Errorf("sent %q", got)
	}
}

// TestMouseInGapIsNoTyping: a moved mouse does not call the switch off.
func TestMouseInGapIsNoTyping(t *testing.T) {
	s := gapSession(t)
	s.send([]byte("\x1b[<35;1;1M"))
	s.hookEvent(hookEvent{name: exitEnterCheck})
	if s.sw.to == "" {
		t.Error("the switch was cancelled")
	}
	if got := sentTo(s); got != "\x1b[<35;1;1M\r" {
		t.Errorf("sent %q", got)
	}
}

// TestLostReply (M3): an ask whose reply never comes must not block switching
// in silence: a turn end tells the person, and picking again goes on.
func TestLostReply(t *testing.T) {
	s := openCodeSession(t)
	t.Cleanup(s.closePicker)
	s.setSwitch(pendingSwitch{to: adapter.Claude, picked: true})
	s.hookEvent(hookEvent{name: hook.EventPermissionAsked, sessionID: "ses_a", request: "r1"})
	s.offerSwitch()
	if s.open {
		t.Fatal("told while the dialog may well be up, with no turn end")
	}
	s.hookEvent(hookEvent{name: "StopCancelled", sessionID: "ses_a"})
	s.send([]byte("\r")) // clears held, as typing does
	s.hookEvent(hookEvent{name: "Stop", sessionID: "ses_a"})
	s.lastIn = time.Time{}
	s.lastOut.Store(0)
	s.offerSwitch()
	if !s.open || !strings.Contains(strings.Join(s.pk.lines, " "), "pick claude again") {
		t.Fatalf("not told: open %v, %q", s.open, s.pk.lines)
	}
	if s.sw.typed || !s.askOpen() {
		t.Fatal("an ask must not be dropped until the person says so")
	}
	s.closePicker()
	s.releaseAsks()
	s.offerSwitch()
	if s.askOpen() || s.held || !s.atPrompt {
		t.Fatalf("not released: asked %v, held %v, atPrompt %v", s.asked, s.held, s.atPrompt)
	}
}

// TestNewAskIsNotStale: an ask after a turn end starts over.
func TestNewAskIsNotStale(t *testing.T) {
	s := openCodeSession(t)
	s.askStale, s.askTold = true, true
	s.hookEvent(hookEvent{name: hook.EventPermissionAsked, sessionID: "ses_a", request: "r2"})
	if s.askStale || s.askTold {
		t.Fatalf("stale %v, told %v", s.askStale, s.askTold)
	}
}

// TestSlashCommandLeavesNoText (L6): /models and Enter left the session dirty
// for good on OpenCode, so a switch waited on text that was not there. A popup
// that fills in a name, as on the other vendors, stays unknown.
func TestSlashCommandLeavesNoText(t *testing.T) {
	cases := []struct {
		name  string
		v     adapter.Vendor
		typed string
		dirty bool
	}{
		{"a command on OpenCode", adapter.OpenCode, "/models\r", false},
		{"a file token on OpenCode", adapter.OpenCode, "@foo\r", true},
		{"a command on Claude", adapter.Claude, "/models\r", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openCodeSession(t)
			s.o.Vendor = c.v
			s.send([]byte(c.typed))
			if s.dirty != c.dirty {
				t.Fatalf("dirty = %v", s.dirty)
			}
		})
	}
}

// TestSignalArmsTheKill (M7): a SIGTERM to aggy, like a closed terminal, must
// not leave a session OpenCode ignores it in.
func TestSignalArmsTheKill(t *testing.T) {
	old := hangupWait
	t.Cleanup(func() { hangupWait = old })
	hangupWait = time.Millisecond
	for _, c := range []struct {
		v    adapter.Vendor
		want bool
	}{{adapter.OpenCode, true}, {adapter.Claude, false}} {
		s := openCodeSession(t)
		s.o.Vendor = c.v
		got := make(chan syscall.Signal, 1)
		s.killAfterHangup(childSignals(func(sig syscall.Signal) error { got <- sig; return nil }))
		select {
		case sig := <-got:
			if !c.want || sig != syscall.SIGKILL {
				t.Errorf("%s: got %v", c.v, sig)
			}
		case <-time.After(100 * time.Millisecond):
			if c.want {
				t.Errorf("%s: no kill", c.v)
			}
		}
		s.stopHangupKill()
	}
}

// TestDialogKeysAreNotComposerText: live, an Esc that rejected a permission
// left the switch waiting on "text you have not sent", for good.
func TestDialogKeysAreNotComposerText(t *testing.T) {
	for _, keys := range []string{"\x1b", "\x1b[C", "no thanks\r"} {
		s := openCodeSession(t)
		s.hookEvent(hookEvent{name: hook.EventPermissionAsked, sessionID: "ses_a", request: "r1"})
		s.send([]byte(keys))
		if s.dirty || !s.lenKnown || s.compLen != 0 {
			t.Errorf("%q: dirty %v, lenKnown %v, compLen %d", keys, s.dirty, s.lenKnown, s.compLen)
		}
		s.hookEvent(hookEvent{name: hook.EventPermissionReplied, sessionID: "ses_a", request: "r1"})
		s.send([]byte("x"))
		if !s.dirty {
			t.Errorf("%q: typing after the reply is composer text", keys)
		}
	}
}

// TestOpenCodeReplyClearsHoldWithoutTyping: a click answers a dialog with
// no typing, and only a reply can tell.
func TestOpenCodeReplyClearsHoldWithoutTyping(t *testing.T) {
	s := openCodeSession(t)
	s.hookEvent(hookEvent{name: hook.EventPermissionAsked, sessionID: "ses_a", request: "r1"})
	s.send([]byte("\x1b[<0;5;5M\x1b[<0;5;5m"))
	if !s.held {
		t.Fatal("a mouse report should not count as an answer")
	}
	s.hookEvent(hookEvent{name: hook.EventPermissionReplied, sessionID: "ses_a", request: "r1"})
	s.hookEvent(hookEvent{name: "Stop", sessionID: "ses_a"})
	if !s.atPrompt {
		t.Fatal("the turn end should count once the reply came")
	}
}

// TestMouseIsNotTyping: on the full screen, a moved mouse must not mark
// text as unsent, or hold off a switch by looking like recent typing.
func TestMouseIsNotTyping(t *testing.T) {
	s := openCodeSession(t)
	before := s.lastIn
	s.send([]byte("\x1b[<35;10;5M"))
	if s.dirty || s.inputSinceTurn || s.lastIn != before {
		t.Fatalf("dirty %v, input %v, lastIn moved %v", s.dirty, s.inputSinceTurn, s.lastIn != before)
	}
	if got := sentTo(s); got != "\x1b[<35;10;5M" {
		t.Fatalf("the CLI should still get the report, got %q", got)
	}
}

// TestPromptIgnoresMouse: a moved mouse does not close aggy's confirmation.
func TestPromptIgnoresMouse(t *testing.T) {
	s := openCodeSession(t)
	s.input = func([]byte) { t.Fatal("the report reached the CLI's input") }
	s.setSwitch(pendingSwitch{to: adapter.Claude})
	s.pk.ask(adapter.Claude, "aggy: confirm")
	s.openPicker()
	t.Cleanup(s.closePicker)
	s.pickerInput([]byte("\x1b[<35;10;5M"))
	if !s.open || s.pk.confirm == "" {
		t.Fatal("the confirmation closed on a mouse report")
	}
}

func TestGuardReport(t *testing.T) {
	s := openCodeSession(t)
	s.hookEvent(hookEvent{name: hook.EventGuardLoaded})
	if !s.guardSeen || s.held {
		t.Fatalf("the report must count and hold nothing: seen %v, held %v", s.guardSeen, s.held)
	}
	s.hookEvent(hookEvent{name: guardCheck})
	if s.open {
		t.Fatal("a note opened though the guard reported")
	}
	s = openCodeSession(t)
	s.hookEvent(hookEvent{name: guardCheck})
	t.Cleanup(s.closePicker)
	if !s.open || !s.pk.note || !strings.Contains(strings.Join(s.pk.lines, " "), "no report from its guard") {
		t.Fatalf("no warning: open %v, lines %q", s.open, s.pk.lines)
	}
}

// TestGuardWatch fires for OpenCode only.
func TestGuardWatch(t *testing.T) {
	old := guardWait
	guardWait = time.Millisecond
	t.Cleanup(func() { guardWait = old })
	for _, c := range []struct {
		v    adapter.Vendor
		want bool
	}{{adapter.OpenCode, true}, {adapter.Claude, false}} {
		s := &session{o: Options{Vendor: c.v, RunDir: "/run"}, events: make(chan hookEvent, 1), quit: make(chan struct{})}
		s.startGuardWatch()
		select {
		case ev := <-s.events:
			if !c.want || ev.name != guardCheck {
				t.Errorf("%s: got %+v", c.v, ev)
			}
		case <-time.After(100 * time.Millisecond):
			if c.want {
				t.Errorf("%s: no check", c.v)
			}
		}
		close(s.quit)
	}
}

// TestOpenCodeEventsOnlyOnOpenCode: under another vendor these names hold
// like any unknown hook event, as before.
func TestOpenCodeEventsOnlyOnOpenCode(t *testing.T) {
	s := openCodeSession(t)
	s.o.Vendor = adapter.Claude
	s.hookEvent(hookEvent{name: hook.EventGuardLoaded})
	if s.guardSeen || !s.held {
		t.Fatalf("seen %v, held %v", s.guardSeen, s.held)
	}
}

// childSignals is a Child that only takes signals.
type childSignals func(syscall.Signal) error

func (f childSignals) Signal(sig syscall.Signal) error { return f(sig) }
