//go:build unix

package supervisor

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
)

// The keys a person may send, one per read.
const (
	kDown  = "\x1b[B"
	kUp    = "\x1b[A"
	kLeft  = "\x1b[D"
	kTab   = "\t"
	kBS    = "\x7f"
	kEsc   = "\x1b"
	kCtrlU = "\x15"
)

// oneRead starts an entry of sendKeys that goes, with the Enter, in one read.
const oneRead = "\x00"

// sendKeys sends each entry as its own read, then an Enter alone; an entry of
// printable text is typed one key per read.
func sendKeys(s *session, keys []string) {
	for _, k := range keys {
		if rest, ok := strings.CutPrefix(k, oneRead); ok {
			s.send([]byte(rest + "\r"))
			return
		}
		if strings.HasPrefix(k, "\x1b") || len(k) == 1 {
			s.send([]byte(k))
			continue
		}
		typeKeys(s, k)
	}
	s.send([]byte("\r"))
}

// TestRR121ForgedAfterSlashEnter (T6975, from R121's review): a forged
// prompt-command sent just after the person's own Enter of a / line that is
// none of aggy's (a skill or command that starts a turn, or /clear) names
// nothing they typed: it is refused, opens nothing, and leaves atPrompt as it
// was.
func TestRR121ForgedAfterSlashEnter(t *testing.T) {
	for _, first := range []string{"/deploy", "/review", "/clear", "/compact"} {
		for _, forged := range []string{"/aggy doctor", "/aggy", "/inbox", "/switch grok"} {
			t.Run(first+" "+forged, func(t *testing.T) {
				s, _ := viewSess(t, adapter.Claude, "")
				s.turnRan = true
				typeKeys(s, first)
				s.send([]byte("\r"))
				before := s.atPrompt
				err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: forged})
				if codeOf(err, diag.Code{}) != diag.CodeCommandNoEnter {
					t.Fatalf("err %v, want %s", err, diag.CodeCommandNoEnter)
				}
				if s.open || s.sw.to != "" {
					t.Fatal("a forged command opened something")
				}
				if s.atPrompt != before {
					t.Errorf("atPrompt %v -> %v", before, s.atPrompt)
				}
			})
		}
	}
}

// TestRR121MenuPickWithArrowsRefused (T6975, from R121's review; the name is
// the review's): a real pick from the CLI's menu made with the arrow keys, or
// completed with Tab, is now taken from the hook, as a typed prefix and Enter
// was.
func TestRR121MenuPickWithArrowsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		keys []string
		cmd  string
	}{
		{"typed prefix + Enter", []string{"/sw"}, "/switch"},
		{"arrows then Enter", []string{"/s", kDown, kDown}, "/switch"},
		{"Tab then args", []string{"/sw", kTab, "grok"}, "/switch grok"},
		{"Tab, /inbox", []string{"/in", kTab}, "/inbox"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.turnRan = true
			sendKeys(s, c.keys)
			if err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: c.cmd}); err != nil {
				t.Fatalf("the pick was refused: %v", err)
			}
			if !s.open && s.sw.to == "" {
				t.Fatal("taken, and nothing opened")
			}
		})
	}
}

// TestRR121CmdFreshOverDialog (T6975, from R121's review): after Claude's
// /model, which opens a picker, a command typed next goes to the CLI (where a
// dialog may still have the keys) instead of being caught with a Ctrl+U into
// that dialog; after /clear, which opens none, it is still caught.
func TestRR121CmdFreshOverDialog(t *testing.T) {
	for _, c := range []struct {
		first  string
		caught bool
	}{
		{"/model", false},
		{"/config", false},
		{"/permissions", false},
		{"/memory", false},
		{"/status", false},
		{"/help", false},
		{"/usage", false},
		{"/context", false},
		{"/clear", true},
	} {
		t.Run(c.first, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.turnRan = true
			typeKeys(s, c.first)
			s.send([]byte("\r"))
			typeLine(s, "/switch grok")
			got := sentTo(s)
			if caught(got, "/switch grok") != c.caught {
				t.Fatalf("caught %v, want %v: the CLI got %q", !c.caught, c.caught, got)
			}
			if !c.caught && (s.sw.to != "" || strings.Contains(got, "\x15")) {
				t.Errorf("a Ctrl+U or a switch after %s: %q, sw %q", c.first, got, s.sw.to)
			}
		})
	}
}

// TestMenuPickTyped (T6975): a hook's command is taken only when the keys
// before its Enter can have picked it from the CLI's menu: a typed / and at
// least one letter of its name, then Up, Down, or a Tab and its arguments.
// A paste, a cursor move into the text, history recall, an edit in the
// middle, Esc, or text after an arrow spoils it; a bare / matches nothing,
// being a prefix of every command, the person's own too.
func TestMenuPickTyped(t *testing.T) {
	for _, c := range []struct {
		name  string
		keys  []string
		cmd   string
		taken bool
	}{
		{"/ s Down Down", []string{"/", "s", kDown, kDown}, "/switch", true},
		{"/sw Tab grok", []string{"/sw", kTab, "grok"}, "/switch grok", true},
		{"/in Tab", []string{"/in", kTab}, "/inbox", true},
		{"/ag Up", []string{"/ag", kUp}, "/aggy", true},
		{"/ag Tab doctor", []string{"/ag", kTab, "doctor"}, "/aggy doctor", true},
		{"kitty Down", []string{"/s", "\x1b[1;1B", "\x1b[1;1:3B"}, "/switch", true},
		{"Backspace at the end", []string{"/sx", kBS, "w", kTab, "grok"}, "/switch grok", true},
		{"Backspace in the args", []string{"/sw", kTab, "grox", kBS, "k"}, "/switch grok", true},
		{"typed whole, one read with its Enter", []string{oneRead + "/switch grok"}, "/switch grok", true},
		{"forged /aggy after /deploy", []string{"/deploy"}, "/aggy", false},
		{"forged args after a Tab", []string{"/sw", kTab, "grok"}, "/switch codex", false},
		{"forged args after a pick", []string{"/sw", kDown}, "/switch grok", false},
		{"bare / Down for /switch", []string{"/", kDown}, "/switch", false},
		{"bare / Down Down for /aggy", []string{"/", kDown, kDown}, "/aggy", false},
		{"bare / Tab", []string{"/", kTab}, "/inbox", false},
		{"/s names no other", []string{"/s", kDown}, "/aggy", false},
		{"paste", []string{"\x1b[200~/sw\x1b[201~", kTab}, "/switch", false},
		{"left arrow", []string{"/sw", kLeft}, "/switch", false},
		{"edit in the middle", []string{"/sx", kLeft, kBS, "w"}, "/switch", false},
		{"history recall", []string{kUp}, "/switch grok", false},
		{"history recall, then text", []string{kUp, "/sw"}, "/switch", false},
		{"text after an arrow", []string{"/s", kDown, "w"}, "/switch", false},
		{"Esc", []string{"/sw", kEsc}, "/switch", false},
		{"Ctrl+U", []string{"x", kCtrlU, "/sw", kTab}, "/switch", false},
		{"an arrow after a space", []string{"/switch ", kDown}, "/switch", false},
		{"a second Tab", []string{"/sw", kTab, kTab}, "/switch", false},
		{"an arrow after a Tab", []string{"/sw", kTab, kDown}, "/switch", false},
		{"a space ends the word", []string{"/sw grok"}, "/switch grok", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := viewSess(t, adapter.Claude, "")
			s.turnRan = true
			sendKeys(s, c.keys)
			atPrompt := s.atPrompt
			err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: c.cmd})
			if !c.taken {
				if codeOf(err, diag.Code{}) != diag.CodeCommandNoEnter {
					t.Fatalf("err %v, want %s", err, diag.CodeCommandNoEnter)
				}
				if s.open || s.sw.to != "" || s.atPrompt != atPrompt {
					t.Fatalf("refused, yet open %v, switch %q, atPrompt %v -> %v", s.open, s.sw.to, atPrompt, s.atPrompt)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !s.open && s.sw.to == "" {
				t.Fatal("taken, and nothing opened")
			}
		})
	}
}

// TestForgedSwitchAfterPickAsks (T6975): a /switch the person picked by
// typing /sw, Tab, and a vendor looks the same as one forged for that Enter;
// it is taken, and the status line still asks before anything is typed.
func TestForgedSwitchAfterPickAsks(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	s.turnRan = true
	sendKeys(s, []string{"/sw", kTab, "grok"})
	if err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/switch grok"}); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if s.sw.to != adapter.Grok || !s.sw.hooked || s.sw.keyed {
		t.Fatalf("switch %q hooked %v keyed %v: a hook's switch must ask", s.sw.to, s.sw.hooked, s.sw.keyed)
	}
	if strings.Contains(sentTo(s), "/exit") {
		t.Fatalf("the exit command went before the person confirmed: %q", sentTo(s))
	}
}

// TestToolOnPageAsks (T6975): /aggy tool on shows the question its first run
// asked (a first index's y/N, answered no) with a y step that runs it again
// answering yes, as at a terminal; a run that asked nothing has no y step.
func TestToolOnPageAsks(t *testing.T) {
	s, ran := viewSess(t, adapter.Claude, "Index /r with codegraph? This runs codegraph init /r, which writes .codegraph/ in the repo. [y/N] Nothing indexed or turned on.")
	typeLine(s, "/aggy tool on codegraph")
	settle(t, s)
	if len(*ran) != 1 || strings.Join((*ran)[0], " ") != "tool on codegraph false" {
		t.Fatalf("ran %q", *ran)
	}
	s.viewInput([]byte("y"))
	settle(t, s)
	if len(*ran) != 2 || strings.Join((*ran)[1], " ") != "tool on codegraph true" {
		t.Fatalf("ran %q", *ran)
	}
	s2, ran2 := viewSess(t, adapter.Claude, "rtk for claude: on")
	typeLine(s2, "/aggy tool on rtk")
	settle(t, s2)
	s2.viewInput([]byte("y"))
	if len(*ran2) != 1 || s2.pk.page == nil || s2.pk.page.busy {
		t.Fatalf("y ran a tool on that asked nothing: %q", *ran2)
	}
}
