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

// TestStalePickList (T6941): a list reopened after a pick expired says so.
func TestStalePickList(t *testing.T) {
	old := noTurnPickFresh
	t.Cleanup(func() { noTurnPickFresh = old })
	noTurnPickFresh = 0
	ts, c, done, _ := switchRunOn(t, false, adapter.Claude)
	ts.typeIn(t, "x")
	c.waitGot(t, []byte("x"))
	ts.typeIn(t, "\x1c1")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	time.Sleep(time.Millisecond)
	ts.clearScreen()
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("Your pick of codex is over 10 minutes old"), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	finish(t, c, done)
}

// TestEnterEncodings (T6941): each terminal's Enter is seen, bare or with text.
func TestEnterEncodings(t *testing.T) {
	for _, tt := range []struct {
		in          string
		enter, bare bool
	}{
		{"\x1b[13u", true, true},
		{"\x1b[13;1u", true, true},
		{"\x1b[57414u", true, true},
		{"\x1b[27;1;13~", true, true},
		{"hi\x1b[13u", true, false},
		{"\x1b[14u", false, false},
		{"\x1b[113u", false, false},
	} {
		if got := csiEnter.MatchString(tt.in); got != tt.enter {
			t.Errorf("csiEnter(%q) = %v", tt.in, got)
		}
		if got := bareEnter([]byte(tt.in)); got != tt.bare {
			t.Errorf("bareEnter(%q) = %v", tt.in, got)
		}
	}
	if !bareEnter([]byte("\r")) || bareEnter([]byte("hi\r")) {
		t.Error("CR")
	}
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
		{"\x1b]0;x\rmore\x07", false},
		{"\x1b[?1u", true},
		{"\x1b[<64;10;10M", true},
		{"\x1b[<65;10;10m", true},
		{"\x1b[<0;10;10M", false},
		{"\x1b[<32;10;10M", false},
		{"\x1b[1u", false},
		// T6897.
		{"\x1b[?2004;1$y", true},
		{"\x1b[>0;276;0c", true},
		{"\x1bP>|xterm(388)\x1b\\", true},
		{"\x1b[M`!!", true},
		{"\x1b[Ma!!", true},
		{"\x1b[M !!", false},
		{"\x1b[M !", false},
		{"\x1b[<35;1;1M", true},
		{"\x1b[?1;2u", false},
		{"\x1b[1;2$y", true},
		{"\x1b[>1$y", false},
		// T6982: XTQMODKEYS's and XTSMGRAPHICS's replies (xterm ctlseqs), which
		// no key sends: they counted as typing and spoiled the line after them.
		{"\x1b[>4;2m", true},
		{"\x1b[>4m", true},
		{"\x1b[?1;0;256S", true},
		{"\x1b[?2;0;640;480S", true},
		{"\x1b[1;2S", false}, // Shift+F4
		{"\x1b[m", false},
		{"\x1b[4m", false},
	} {
		if got := terminalReport([]byte(tt.in)); got != tt.want {
			t.Errorf("terminalReport(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
