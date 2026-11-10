//go:build unix

package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// caught reports whether the line typed last, ended by its Enter, stayed out
// of the CLI: what it got does not end in the line and its Enter.
func caught(got, line string) bool { return !strings.HasSuffix(got, line+"\r") }

// TestLongCommandCaught (R121): a command up to sessioncmd's 256 bytes is caught
// from the keys; the tracker once stopped at 32.
func TestLongCommandCaught(t *testing.T) {
	for _, line := range []string{
		"/switch codex --model gpt-5.1-codex-max",
		"/aggy diag 01JC3Z8Q5V7M4N2P6R8T0W2Y4A",
		"/aggy inbox 0123456789abcdef --all",
	} {
		t.Run(line, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.turnRan = true
			if _, ok := sessioncmd.Parse(adapter.Claude, line); !ok {
				t.Fatalf("sessioncmd does not parse %q", line)
			}
			typeLine(s, line)
			if got := sentTo(s); !caught(got, line) || !s.open && s.sw.to == "" {
				t.Fatalf("not caught: the CLI got %q", got)
			}
		})
	}
	t.Run("past 256 bytes", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		line := "/aggy diag " + strings.Repeat("a", sessioncmd.MaxLine)
		typeLine(s, line)
		if got := sentTo(s); caught(got, line) {
			t.Fatalf("caught a line past the limit: %q", got)
		}
	})
}

// TestForgedHookRefused (R121): a prompt-command that goes with no Enter of a /
// command at the prompt just before it (a forged one, mid-turn or after keys)
// is refused with a code, opens nothing, takes no key, and leaves atPrompt as
// it was; a second one for the same Enter is refused too.
func TestForgedHookRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(s *session)
		cmd   string
	}{
		{"keys typed since the Enter", func(s *session) {
			s.send([]byte("x"))
			s.send([]byte("\r"))
			s.enterAt = time.Now().Add(-time.Minute)
			typeKeys(s, "hel")
		}, "/aggy"},
		{"long after the Enter", func(s *session) {
			typeKeys(s, "/ag")
			s.send([]byte("\r"))
			s.enterAt = time.Now().Add(-time.Minute)
		}, "/aggy doctor"},
		{"a message's Enter, mid-turn", func(s *session) {
			typeKeys(s, "fix the bug")
			s.send([]byte("\r"))
		}, "/inbox"},
		{"no Enter at all", func(s *session) {}, "/switch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.turnRan = true
			c.setup(s)
			atPrompt := s.atPrompt
			err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: c.cmd})
			if codeOf(err, diag.Code{}) != diag.CodeCommandNoEnter {
				t.Fatalf("err %v, want %s", err, diag.CodeCommandNoEnter)
			}
			if s.open || s.sw.to != "" {
				t.Fatal("a forged command opened something")
			}
			if s.atPrompt != atPrompt {
				t.Errorf("atPrompt %v -> %v", atPrompt, s.atPrompt)
			}
			s.send([]byte("lo"))
			if got := sentTo(s); !strings.HasSuffix(got, "lo") {
				t.Errorf("the next keys did not reach the CLI: %q", got)
			}
		})
	}
	t.Run("one per Enter", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Claude, "")
		typeKeys(s, "/ag")
		s.send([]byte("\r"))
		if err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/aggy"}); err != nil {
			t.Fatalf("the menu pick was refused: %v", err)
		}
		// The same prompt reported again (UserPromptExpansion) blocks too.
		if err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/aggy"}); err != nil {
			t.Fatalf("the repeat was refused: %v", err)
		}
		menu := s.pk.menu
		if err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/inbox"}); codeOf(err, diag.Code{}) != diag.CodeCommandNoEnter {
			t.Fatalf("a second command for one Enter: err %v", err)
		}
		if s.pk.menu != menu || s.pk.page != nil {
			t.Error("the second command replaced the list on screen")
		}
	})
}

// typeKeys sends keys one per read, as a person types them.
func typeKeys(s *session, keys string) {
	for _, k := range strings.Split(keys, "") {
		s.send([]byte(k))
	}
}

// TestAfterLocalCommand (R121): after a Claude command its docs list as
// starting no turn, typed whole, the next command typed in full is caught.
// On the other CLIs no such list is recorded, so the composer stays in doubt
// and the next command goes to the CLI until a turn end (docs/design.md
// section 5, a known limit).
func TestAfterLocalCommand(t *testing.T) {
	for _, c := range []struct {
		on          adapter.Vendor
		first, next string
		caught      bool
	}{
		{adapter.Claude, "/clear", "/switch grok", true},
		{adapter.Claude, "/model", "/inbox", true},
		{adapter.Claude, "/compact", "/inbox", false}, // may run a turn
		{adapter.Codex, "/model", "$switch grok", false},
		{adapter.Codex, "/status", "/switch grok", false},
		{adapter.Grok, "/clear", "/switch codex", false},
	} {
		t.Run(string(c.on)+c.first, func(t *testing.T) {
			s, _ := viewSess(t, c.on, "")
			s.turnRan = true
			typeKeys(s, c.first)
			s.send([]byte("\r"))
			typeLine(s, c.next)
			if got := sentTo(s); caught(got, c.next) != c.caught {
				t.Fatalf("caught %v, want %v: the CLI got %q", !c.caught, c.caught, got)
			}
		})
	}
}

// TestFillAfterHook (R121): after /aggy picked from the CLI's menu and taken
// by the hook, picking "aggy accept " fills the composer, and the line then
// typed in full is caught as keyboard-only, never reaching the CLI.
func TestFillAfterHook(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	typeKeys(s, "/ag")
	s.send([]byte("\r")) // the CLI's menu picks /aggy and submits it
	if err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/aggy"}); err != nil {
		t.Fatal(err)
	}
	at := -1
	for i, it := range s.pk.menu.items {
		if it.line == "/aggy accept " {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("no accept item")
	}
	for range at {
		s.pickerInput([]byte("j"))
	}
	s.pickerInput([]byte("\r"))
	if s.pk.page != nil || s.open {
		t.Fatalf("the fill was refused: %+v", s.pk.page)
	}
	typeLine(s, "0123456789abcdef")
	got := sentTo(s)
	if !strings.HasSuffix(got, "/aggy accept 0123456789abcdef\x15") {
		t.Fatalf("the CLI got %q, want the filled line, its Ctrl+U, and no Enter", got)
	}
	if !s.open || s.pk.page == nil || len(s.pk.page.confirm) == 0 {
		t.Fatal("not caught as a keyboard-only command with its confirmation")
	}
}

// TestNoCatchOnAltScreen (R121): Claude and Grok draw inline, so on the
// alternate screen their keys go to an editor or a pager, and nothing is
// caught there; Codex draws there all the time, and is caught.
func TestNoCatchOnAltScreen(t *testing.T) {
	for _, c := range []struct {
		on     adapter.Vendor
		line   string
		caught bool
	}{
		{adapter.Claude, "/inbox", false},
		{adapter.Grok, "/inbox", false},
		{adapter.Codex, "$inbox", true},
	} {
		t.Run(string(c.on), func(t *testing.T) {
			s, _ := viewSess(t, c.on, "")
			s.outMu.Lock()
			s.alt.on = true
			s.outMu.Unlock()
			typeLine(s, c.line)
			if got := sentTo(s); caught(got, c.line) != c.caught {
				t.Fatalf("caught %v, want %v: %q", !c.caught, c.caught, got)
			}
		})
	}
}
