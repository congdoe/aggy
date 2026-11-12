//go:build unix

package supervisor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// cmdSess is a session at its prompt after a counted turn end, which can
// switch to grok or codex.
func cmdSess(t *testing.T, v adapter.Vendor) *session {
	t.Helper()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	s := &session{o: Options{Vendor: v, Out: out, SessionID: func() string { return childSID },
		Vendors: []adapter.Vendor{adapter.Grok, adapter.Codex, adapter.Claude}, CanSwitch: func(adapter.Vendor) error { return nil }},
		t: &tty{fd: -1}, pk: &picker{key: 0x1c}, toChild: newByteQueue(maxQueued), atPrompt: true, lenKnown: true,
		quit: make(chan struct{}), events: make(chan hookEvent, 4)}
	s.pk.vendors = s.o.Vendors
	s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})
	return s
}

// TestCatchCommand (T6908): a command typed in full is caught on its Enter,
// which never reaches the CLI; the composer gets Ctrl+U instead. Anything the
// tracker does not know whole goes through as typed.
func TestCatchCommand(t *testing.T) {
	type tc struct {
		name   string
		on     adapter.Vendor
		keys   []string
		caught bool
		sent   string // what the CLI got, when caught
		to     adapter.Vendor
	}
	each := func(s string) []string { return strings.Split(s, "") }
	cases := []tc{
		{"typed in full", adapter.Claude, append(each("/switch grok"), "\r"), true, "/switch grok\x15", adapter.Grok},
		{"with options", adapter.Claude, append(each("/switch codex --brief"), "\r"), true, "/switch codex --brief\x15", adapter.Codex},
		{"Codex's $ form", adapter.Codex, append(each("$switch grok"), "\r"), true, "$switch grok\x15", adapter.Grok},
		{"Codex's / form", adapter.Codex, append(each("/switch grok"), "\r"), true, "/switch grok\x15", adapter.Grok},
		{"$ is not Claude's", adapter.Claude, append(each("$switch grok"), "\r"), false, "", ""},
		{"typed with its Enter in one read", adapter.Claude, []string{"/switch grok\r"}, false, "", ""},
		{"pasted", adapter.Claude, []string{"\x1b[200~/switch grok\x1b[201~", "\r"}, false, "", ""},
		{"pasted, split markers", adapter.Claude, []string{"\x1b[200", "~/switch grok\x1b[201~", "\r"}, false, "", ""},
		{"an edit in it", adapter.Claude, append(each("/switch grox"), "\x7f", "k", "\r"), false, "", ""},
		{"a cursor key in it", adapter.Claude, append(each("/switch grok"), "\x1b[D", "\r"), false, "", ""},
		{"history recall", adapter.Claude, []string{"\x1b[A", "\r"}, false, "", ""},
		{"after a popup's Enter", adapter.Claude, append(append(each("@fi"), "\r"), append(each("/switch grok"), "\r")...), false, "", ""},
		{"after a newline", adapter.Claude, append(append(each("x\\"), "\r"), append(each("/switch grok"), "\r")...), false, "", ""},
		{"after an Esc", adapter.Claude, append([]string{"\x1b"}, append(each("/switch grok"), "\r")...), false, "", ""},
		{"Shift+Enter", adapter.Claude, append(each("/switch grok"), "\x1b[13;2u"), false, "", ""},
		{"not a command", adapter.Claude, append(each("/switchy"), "\r"), false, "", ""},
		{"a borrow is a model turn", adapter.Claude, append(each("/borrow news x"), "\r"), false, "", ""},
		{"after a sent message", adapter.Claude, append(append(each("hi"), "\r"), append(each("/switch grok"), "\r")...), true, "hi\r/switch grok\x15", adapter.Grok},
		{"kitty keys", adapter.Claude, append(kittyKeys("/switch grok"), "\x1b[13u", "\x1b[13;1:3u"), true, string(kittyRaw("/switch grok")) + "\x1b[117;5u", adapter.Grok},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := cmdSess(t, c.on)
			for _, k := range c.keys {
				s.send([]byte(k))
			}
			got := sentTo(s)
			if !c.caught {
				if s.sw.to != "" || !strings.HasSuffix(got, string(c.keys[len(c.keys)-1])) {
					t.Fatalf("sent %q, switch to %q: want the keys through and no switch", got, s.sw.to)
				}
				return
			}
			if got != c.sent {
				t.Fatalf("sent %q, want %q", got, c.sent)
			}
			if s.sw.to != c.to || !s.sw.picked {
				t.Fatalf("switch %+v, want one to %s", s.sw, c.to)
			}
			if s.sw.keyed != holdSignal(c.on) {
				t.Errorf("keyed = %v on %s, want %v", s.sw.keyed, c.on, holdSignal(c.on))
			}
			// A message sent before it started a turn, which the command does not end.
			if s.knownText() || s.doubt() || s.atPrompt == strings.Contains(c.sent, "\r") {
				t.Errorf("knownText %v doubt %v atPrompt %v: want an empty composer at the prompt", s.knownText(), s.doubt(), s.atPrompt)
			}
		})
	}
}

func kittyKeys(s string) []string {
	var out []string
	for _, r := range s {
		out = append(out, string(kittyRaw(string(r))))
	}
	return out
}

func kittyRaw(s string) []byte {
	var b []byte
	for _, r := range s {
		b = append(b, []byte("\x1b["+itoa(int(r))+"u")...)
	}
	return b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

// TestCatchLeavesNoTurnInput (T6908): a caught command and its Ctrl+U typed
// nothing, so the prompt still counts as one no input reached since the turn
// end, and a switch it queues does not wait for a turn end that never comes.
func TestCatchLeavesNoTurnInput(t *testing.T) {
	s := cmdSess(t, adapter.Claude)
	if s.inputSinceTurn {
		t.Fatal("input before any key")
	}
	for _, k := range strings.Split("/switch grok", "") {
		s.send([]byte(k))
	}
	s.send([]byte("\r"))
	if s.inputSinceTurn || !s.atPrompt || s.mayBeRunning() {
		t.Errorf("inputSinceTurn %v atPrompt %v: a caught command must leave the prompt as it was", s.inputSinceTurn, s.atPrompt)
	}
}

// TestPlainEnter: only an unmodified Enter press counts.
func TestPlainEnter(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"\r", true}, {"\x1b[13u", true}, {"\x1b[13;1u", true}, {"\x1b[13;65u", true}, {"\x1b[57414u", true},
		{"\x1b[13;2u", false}, {"\x1b[13;3u", false}, {"\x1b[13;1:3u", false}, {"\n", false}, {"\r\r", false},
		{"\x1b[27;1;13~", true}, {"\x1b[27;2;13~", false},
	} {
		if got := plainEnter([]byte(c.in)); got != c.want {
			t.Errorf("plainEnter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTypedSwitchConfirms (T6908): a /switch typed at Claude's prompt types the
// exit command at the quiet turn end with no status line, as the command was the
// confirmation; on Codex, which signals no approval prompt, the status line asks.
// Claude's entry is the recorded live check (T6982): its Ctrl+U byte, typed for
// a plain Enter, is checked; the CSI u form, typed for a kitty Enter, is not, so
// that switch asks.
func TestTypedSwitchConfirms(t *testing.T) {
	for _, c := range []struct {
		on    adapter.Vendor
		check string // "recorded": adapter.ClearLineChecked; else stubbed yes or no
		enter string
		asks  bool
	}{
		{adapter.Claude, "no", "\r", true},
		{adapter.Claude, "yes", "\r", false},
		{adapter.Codex, "yes", "\r", true}, // no approval signal
		{adapter.Claude, "recorded", "\r", false},
		{adapter.Claude, "recorded", "\x1b[13u", true},
		{adapter.Claude, "yes", "\x1b[13u", true}, // the CSI u Ctrl+U has no check anywhere
		{adapter.Codex, "recorded", "\r", true},
	} {
		t.Run(fmt.Sprintf("%s checked %s enter %q", c.on, c.check, c.enter), func(t *testing.T) {
			if c.check != "recorded" {
				was := clearsLine
				clearsLine = func(adapter.Vendor) bool { return c.check == "yes" }
				t.Cleanup(func() { clearsLine = was })
			}
			ts, ch, done, _ := switchRunTurned(t, c.on)
			for _, k := range strings.Split("/switch grok", "") {
				ts.typeIn(t, k)
			}
			ch.waitGot(t, []byte("/switch grok"))
			time.Sleep(30 * time.Millisecond)
			ts.typeIn(t, c.enter)
			if c.asks {
				poll(t, "the status line", []byte("-> grok. Ctrl+"), ts.screen)
				if bytes.Contains(ch.received(), []byte("/exit")) {
					t.Fatalf("%s got the exit command before the person confirmed: %q", c.on, ch.received())
				}
				if c.on == adapter.Claude && bytes.Contains(ts.screen(), []byte("is unverified")) {
					t.Errorf("Claude's /exit has a live check, yet the line says it is unverified: %q", ts.screen())
				}
				ts.typeIn(t, "\x1b")
				time.Sleep(escTimeout + 60*time.Millisecond)
				finish(t, ch, done)
				return
			}
			poll(t, "the exit command", []byte("/switch grok\x15/exit"), ch.received)
			if bytes.Contains(ts.screen(), []byte("Esc stays")) {
				t.Errorf("a status line came for a typed switch: %q", ts.screen())
			}
			if bytes.Contains(ch.received(), []byte("grok\r")) {
				t.Errorf("the command's Enter reached the CLI: %q", ch.received())
			}
			ch.quit(0)
			select {
			case ex := <-done:
				if ex.SwitchTo != adapter.Grok {
					t.Errorf("exit %+v, want a switch to grok", ex)
				}
			case <-time.After(waitLimit):
				t.Fatal("Run did not return")
			}
		})
	}
}

// TestHookPickTellsNoTurn (T6982): on a fresh launch, a /switch the prompt hook
// took, then a pick from the list it opened, is told at once that no turn has
// ended: the blocked prompt started nothing, so nothing may be running.
// Before, the line's keys counted as input and the note never came.
func TestHookPickTellsNoTurn(t *testing.T) {
	s := cmdSess(t, adapter.Claude)
	s.turnRan, s.inputSinceTurn = false, false
	s.alt.on = true // the key catch lets it through, as in the owner's run
	for _, k := range append(strings.Split("/switch", ""), "\r") {
		s.send([]byte(k))
	}
	c, _ := sessioncmd.Parse(adapter.Claude, "/switch")
	if err := s.hookCommand(c, "/switch"); err != nil {
		t.Fatal(err)
	}
	if !s.open {
		t.Fatal("the list did not open")
	}
	s.pickerInput([]byte("1"))
	if s.sw.to != adapter.Grok {
		t.Fatalf("switch %+v, want one to grok", s.sw)
	}
	s.lastIn = time.Now().Add(-time.Minute)
	s.offerSwitch()
	if !s.open || !s.sw.noteShown {
		t.Errorf("open %v noteShown %v inputSinceTurn %v: want the no-turn note", s.open, s.sw.noteShown, s.inputSinceTurn)
	}
}
