//go:build unix

package supervisor

import (
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestStalePick (T6941): a pick told to quit goes stale, so a quit long after
// ends aggy with a line rather than landing in another CLI.
func TestStalePick(t *testing.T) {
	old := noTurnPickFresh
	t.Cleanup(func() { noTurnPickFresh = old })
	noTurnPickFresh = 0
	ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
	ts.typeIn(t, "\r")
	c.waitGot(t, []byte("\r"))
	ts.typeIn(t, "\x1c1")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	time.Sleep(time.Millisecond)
	c.quit(0)
	wantExit(t, done, Exit{})
	poll(t, "why", []byte("you picked codex over 10 minutes ago, so aggy did not switch to codex."), ts.screen)
}

// TestTerminalReport (T6941): only whole replies a terminal sends a CLI are not
// input; a key or a paste always is.
func TestTerminalReport(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{
		{"\x1b[I", true},
		{"\x1b[O", true},
		{"\x1b[12;40R", true},
		{"\x1b[?62;4c", true},
		{"\x1b[0n", true},
		{"\x1b[?2026;2$y", true},
		{"\x1b]11;rgb:0000/0000/0000\x07", true},
		{"\x1b]11;rgb:0000/0000/0000\x1b\\", true},
		{"\x1bP1$r0m\x1b\\", true},
		{"\x1b[I\x1b[12;40R", true},
		{"", false},
		{"x", false},
		{"\r", false},
		{"\x1b", false},
		{"\x1b[A", false},
		{"\x1b[13u", false},
		{"\x1b[200~hi\x1b[201~", false},
		{"\x1b[Ix", false},
		{"x\x1b[I", false},
		{"\x1b]11;rgb", false},
	} {
		if got := terminalReport([]byte(tt.in)); got != tt.want {
			t.Errorf("terminalReport(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
