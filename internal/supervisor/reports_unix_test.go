//go:build unix

package supervisor

import (
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestSendNotTyped (T6897): what the terminal says on its own, a click, and an
// Esc put no text in the composer; a click still counts as input.
func TestSendNotTyped(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		dirty  bool
		input  bool // it counts as input to the CLI
		lastIn bool // it counts as the user typing
	}{
		{"focus out", "\x1b[O", false, false, false},
		{"focus in", "\x1b[I", false, false, false},
		{"cursor position", "\x1b[24;80R", false, false, false},
		{"device attributes", "\x1b[?62;22c", false, false, false},
		{"secondary device attributes", "\x1b[>0;276;0c", false, false, false},
		{"mode report", "\x1b[?2026;2$y", false, false, false},
		{"background colour", "\x1b]11;rgb:0000/0000/0000\x1b\\", false, false, false},
		{"terminal version", "\x1bP>|xterm(388)\x1b\\", false, false, false},
		{"sgr wheel", "\x1b[<64;10;10M", false, false, false},
		{"x10 wheel", "\x1b[M`!!", false, false, false},
		{"sgr hover", "\x1b[<35;10;10M", false, false, false},
		{"x10 hover", "\x1b[MC!!", false, false, false},
		{"sgr click", "\x1b[<0;10;10M", false, true, true},
		{"x10 click", "\x1b[M !!", false, true, true},
		{"esc", "\x1b", false, true, true},
		{"kitty esc", "\x1b[27u", false, true, true},
		{"text", "x", true, true, true},
		{"up may bring back history", "\x1b[A", true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{toChild: newByteQueue(maxQueued), lenKnown: true, atPrompt: true}
			s.send([]byte(tt.in))
			if s.dirty != tt.dirty {
				t.Errorf("dirty = %v, want %v", s.dirty, tt.dirty)
			}
			if s.inputSinceTurn != tt.input {
				t.Errorf("inputSinceTurn = %v, want %v", s.inputSinceTurn, tt.input)
			}
			if got := !s.lastIn.IsZero(); got != tt.lastIn {
				t.Errorf("lastIn set = %v, want %v", got, tt.lastIn)
			}
		})
	}
}

// TestEscThenEnter (T6897): an Esc long before an Enter is no Option+Enter, so
// interrupting a turn and later pressing Enter leaves nothing unsent.
func TestEscThenEnter(t *testing.T) {
	old := splitWait
	splitWait = time.Millisecond
	t.Cleanup(func() { splitWait = old })
	s := &session{toChild: newByteQueue(maxQueued), lenKnown: true}
	s.send([]byte("\x1b"))
	time.Sleep(5 * time.Millisecond)
	s.send([]byte("\r"))
	if s.dirty {
		t.Error("the Enter counted as a newline in the composer")
	}
}

// TestTabQueues (T6897): Tab with text on Codex takes the text with it, so
// nothing is left unsent; a popup may have taken it, and another CLI may have
// used it to complete a word, so there the text stays in doubt.
func TestTabQueues(t *testing.T) {
	tests := []struct {
		name     string
		vendor   adapter.Vendor
		atPrompt bool
		chunks   []string
		dirty    bool
		queued   int
	}{
		{"codex, during a turn, one read", adapter.Codex, false, []string{"more\t"}, false, 1},
		{"codex, during a turn, two reads", adapter.Codex, false, []string{"more", "\t"}, false, 1},
		{"codex, kitty tab", adapter.Codex, false, []string{"more", "\x1b[9u"}, false, 1},
		{"codex, idle: it submits", adapter.Codex, true, []string{"more", "\t"}, false, 0},
		{"codex, a popup may take it", adapter.Codex, false, []string{"read @sr", "\t"}, true, 1},
		{"codex, a / popup may take it", adapter.Codex, true, []string{"/mo", "\t"}, true, 0},
		{"codex, more text after it", adapter.Codex, false, []string{"more\tagain"}, true, 1},
		{"codex, text after a Tab", adapter.Codex, false, []string{"more\t", "again"}, true, 1},
		{"codex, Tab in a paste", adapter.Codex, false, []string{"\x1b[200~a\tb"}, true, 1},
		{"claude, it may complete a word", adapter.Claude, false, []string{"more", "\t"}, true, 1},
		{"grok, it may complete a word", adapter.Grok, false, []string{"more", "\t"}, true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &session{o: Options{Vendor: tt.vendor}, toChild: newByteQueue(maxQueued), lenKnown: true, atPrompt: tt.atPrompt}
			for _, c := range tt.chunks {
				s.send([]byte(c))
			}
			if s.dirty != tt.dirty {
				t.Errorf("dirty = %v, want %v", s.dirty, tt.dirty)
			}
			if s.queued != tt.queued {
				t.Errorf("queued = %d, want %d", s.queued, tt.queued)
			}
		})
	}
}

// TestSwitchNotHeldByReports (T6897): the two repros: replies and focus events
// after a finished turn, whole, split across reads, or beside the switch key,
// never make the unsent-text refusal, and the status line asks.
func TestSwitchNotHeldByReports(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{"device attributes", []string{"\x1b[?62;22c"}},
		{"focus out and in", []string{"\x1b[O", "\x1b[I"}},
		{"every reply, one read", []string{"\x1b[24;80R\x1b]11;rgb:0/0/0\x07\x1bP>|x\x1b\\\x1b[>0;1;0c\x1b[?1;2$y\x1b[M`!!"}},
		{"split reply", []string{"\x1b[?6", "2;22c"}},
		{"a click, as Grok's mouse reporting sends", []string{"\x1b[<0;3;3M", "\x1b[<0;3;3m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, c, done, _ := switchRunTurned(t, adapter.Claude)
			for _, ch := range tt.chunks {
				ts.typeIn(t, ch)
				time.Sleep(10 * time.Millisecond)
			}
			ts.typeIn(t, "\x1c2")
			poll(t, "the status line", []byte("-> grok. Ctrl+"), ts.screen)
			never(t, "the refusal", []byte("you typed text"), ts.screen, 50*time.Millisecond)
			finish(t, c, done)
		})
	}
}

// TestPickerIgnoresReportDigits (T6897): a reply split across reads while the
// list is up reaches the CLI whole, and its digits pick nothing.
func TestPickerIgnoresReportDigits(t *testing.T) {
	ts, c, done, _ := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	ts.typeIn(t, "\x1b[?1")
	time.Sleep(5 * time.Millisecond)
	ts.typeIn(t, "2;22c")
	c.waitGot(t, []byte("\x1b[?12;22c"))
	never(t, "a pick", []byte("-> "), ts.screen, 100*time.Millisecond)
	ts.typeIn(t, "\x1b")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	finish(t, c, done)
}

// TestReportWithKeyAtList (T6897): a reply in one read with a key leaves the
// key for the list.
func TestReportWithKeyAtList(t *testing.T) {
	ts, c, done, _ := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	ts.typeIn(t, "\x1b[I2")
	poll(t, "the status line", []byte("-> grok. Ctrl+"), ts.screen)
	c.waitGot(t, []byte("\x1b[I"))
	finish(t, c, done)
}

// TestSplitKeyGoesOn (T6897): a key's escape sequence the filter held goes on,
// even when its end never comes.
func TestSplitKeyGoesOn(t *testing.T) {
	ts, c, done, _ := switchRunTurned(t, adapter.Claude)
	ts.typeIn(t, "\x1b[1")
	c.waitGot(t, []byte("\x1b[1"))
	finish(t, c, done)
}
