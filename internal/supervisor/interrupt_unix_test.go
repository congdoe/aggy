//go:build unix

package supervisor

import (
	"bytes"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// shortInterrupt makes settleInterrupt's quiet gap short for a test.
func shortInterrupt(t *testing.T) {
	t.Helper()
	old := interruptQuiet
	interruptQuiet = 200 * time.Millisecond
	t.Cleanup(func() { interruptQuiet = old })
}

// TestInterruptThenSwitch (T6968, M4): Claude sends no Stop for an
// interrupted turn. A message, then Esc, then quiet, lets a switch picked
// once go on to its status line, in doubt, and only the switch key types the
// exit command.
func TestInterruptThenSwitch(t *testing.T) {
	shortInterrupt(t)
	ts, c, done, _ := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "go on\r")
	c.waitGot(t, []byte("go on\r"))
	time.Sleep(50 * time.Millisecond)
	ts.typeIn(t, "\x1b")
	c.waitGot(t, []byte("go on\r\x1b"))
	ts.typeIn(t, "\x1c2")
	poll(t, "the status line", []byte("-> grok. Ctrl+"), ts.screen)
	if !bytes.Contains(ts.screen(), []byte("can't tell")) {
		t.Errorf("the status line does not say aggy cannot tell: %q", ts.screen())
	}
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Fatal("the exit command went before the person confirmed")
	}
	confirm(t, ts)
	c.waitGot(t, []byte("/exit\r"))
	c.quit(0)
	wantExit(t, done, Exit{SwitchTo: adapter.Grok, Next: Choice{Vendor: adapter.Grok}})
}

// TestInterruptGuards: what keeps the switch waiting as before.
func TestInterruptGuards(t *testing.T) {
	tests := []struct {
		name  string
		after string // typed after the message, before the pick
		hold  bool   // a Notification arrives after the Esc
	}{
		{"no Esc: the turn may still run", "", false},
		{"text after the Esc", "\x1bmore", false},
		{"a hold after the Esc", "\x1b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortInterrupt(t)
			ts, c, done, sock := switchRunTurned(t, adapter.Claude)
			ts.typeIn(t, "go on\r")
			c.waitGot(t, []byte("go on\r"))
			if tt.after != "" {
				time.Sleep(50 * time.Millisecond)
				ts.typeIn(t, tt.after)
				c.waitGot(t, []byte("go on\r"+tt.after))
			}
			if tt.hold {
				_ = request(t, sock, notifyLine)
			}
			ts.typeIn(t, "\x1c2")
			poll(t, "the picker to close", []byte(altScreenOff), ts.screen)
			never(t, tt.name, []byte("-> grok. Ctrl+"), ts.screen, 600*time.Millisecond)
			if bytes.Contains(c.received(), []byte("/exit")) {
				t.Fatal("the exit command went")
			}
			finish(t, c, done)
		})
	}
}

// TestInterruptBeforeFirstTurnEnd: before any counted turn end an Esc changes
// nothing. aggy still never types the exit command into a possible launch
// dialog (T6893), so the list's quit-by-hand words stand.
func TestInterruptBeforeFirstTurnEnd(t *testing.T) {
	shortInterrupt(t)
	ts, c, done, _ := switchRun(t, false)
	ts.typeIn(t, "hi\r")
	c.waitGot(t, []byte("hi\r"))
	time.Sleep(50 * time.Millisecond)
	ts.typeIn(t, "\x1b")
	c.waitGot(t, []byte("hi\r\x1b"))
	ts.typeIn(t, "\x1c2")
	never(t, "before a turn end", []byte("-> grok. Ctrl+"), ts.screen, 600*time.Millisecond)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Fatal("the exit command went before any turn end")
	}
	finish(t, c, done)
}
