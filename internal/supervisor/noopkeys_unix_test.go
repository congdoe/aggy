//go:build unix

package supervisor

import (
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestNoopKeyGuards (#146 review, second round): the empty-composer no-op keys
// hold only where they were checked. Mid-turn a Down counts as a key after the
// Enter (it may answer what that Enter opened); their kitty forms were not
// checked, so they spoil the line; after a ! or # line Claude may still be in
// its bash or memory mode, which Backspace, Delete, or Ctrl+U on an empty
// composer may leave, so those spoil it too, while a Down does not.
func TestNoopKeyGuards(t *testing.T) {
	t.Run("mid-turn counts after the Enter", func(t *testing.T) {
		s := cmdSess(t, adapter.Claude)
		s.o.VendorVersion = checkedClaude
		typeLine(s, "fix the bug")
		s.send([]byte(kDown))
		if s.afterEnter == 0 {
			t.Error("a Down mid-turn did not count as a key after the Enter")
		}
	})
	for _, c := range []struct {
		name   string
		before string // a line sent first, "" for none
		key    string
		caught bool
	}{
		{"kitty Down", "", "\x1b[1;1B", false},
		{"kitty Home", "", "\x1b[1;1H", false},
		{"legacy Down", "", kDown, true},
		{"Backspace after a ! line", "!ls", kBS, false},
		{"Ctrl+U after a # line", "#note", kCtrlU, false},
		{"Delete after a ! line", "!ls", "\x1b[3~", false},
		{"Down after a ! line", "!ls", kDown, false}, // mode_line: no Ctrl+U or /exit typed
		{"nothing after a # line", "#note", "", false},
		{"SS3 Down", "", "\x1bOB", false},
		{"Ctrl+H", "", "\x08", false},
		{"Home as ESC [1~", "", "\x1b[1~", false},
		{"Backspace as CSI u", "", "\x1b[127u", false},
		{"Backspace as CSI u with an event", "", "\x1b[127;1:1u", false},
		{"Ctrl+U as CSI u", "", "\x1b[117;5u", false},
		{"Backspace after a message", "hi", kBS, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			noSettleGap(t)
			s := cmdSess(t, adapter.Claude)
			s.o.VendorVersion = checkedClaude
			if c.before != "" {
				typeLine(s, c.before)
				s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
			}
			s.toChild = newByteQueue(maxQueued)
			if c.key != "" {
				s.send([]byte(c.key))
			}
			typeLine(s, "/switch grok")
			if got := caught(sentTo(s), "/switch grok"); got != c.caught {
				t.Errorf("caught %v, want %v (doubts %v)", got, c.caught, s.lineDoubts())
			}
		})
	}
}
