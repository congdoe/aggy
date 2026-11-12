//go:build unix

package supervisor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
	"github.com/congdoe/aggy/internal/store"
)

// wantAsked checks that a command from a prompt hook the keys did not vouch for
// was blocked (no error: aggy hook blocks the prompt, so no model turn) and
// asked about: the question open, nothing run or switched, atPrompt as it was.
func wantAsked(t *testing.T, s *session, err error, atPrompt bool) {
	t.Helper()
	if err != nil {
		t.Fatalf("err %v, want the prompt blocked and a question", err)
	}
	if !s.open || !s.pk.cmdAsk || s.hookAsk == nil {
		t.Fatalf("no question: open %v cmdAsk %v", s.open, s.pk.cmdAsk)
	}
	if s.sw.to != "" || s.pk.menu != nil || s.pk.page != nil {
		t.Fatalf("ran before the person's key: switch %q menu %v page %v", s.sw.to, s.pk.menu != nil, s.pk.page != nil)
	}
	if s.atPrompt != atPrompt {
		t.Errorf("atPrompt %v -> %v", atPrompt, s.atPrompt)
	}
}

// recAttrs returns the attrs of the records of ev in recs, in order.
func recAttrs(t *testing.T, recs []store.DiagRecord, ev diag.Event) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range recs {
		if r.Event != ev.String() {
			continue
		}
		var a map[string]any
		if err := json.Unmarshal(r.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// pastGuard lets a key at the question count as meant (confirmGuard).
func pastGuard(s *session) { s.openedAt = time.Now().Add(-time.Second) }

// TestOwnerRunReplay (T6982): the owner's two lines in Apple Terminal, replayed.
// First, at launch, a Down on the empty prompt, /switch, a Down, and Enter: the
// key catch lost the line, and the hook's /switch was refused for keys no menu
// pick could be, so the prompt went to the model. Then, after that turn's Stop,
// a Backspace and /switch codex typed in full: refused the same way, with no
// key record at all. Now each prompt is blocked and asked about, and one key
// runs it; nothing reaches the model.
func TestOwnerRunReplay(t *testing.T) {
	noSettleGap(t)
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Claude)
	s.o.Diag = q
	s.input = s.send
	s.turnRan = false // a fresh launch: no Stop yet
	for _, k := range append(append([]string{kDown}, strings.Split("/switch", "")...), kDown, "\r") {
		s.send([]byte(k))
	}
	c, _ := sessioncmd.Parse(adapter.Claude, "/switch")
	wantAsked(t, s, s.hookCommand(c, "/switch"), false)
	pastGuard(s)
	s.pickerInput([]byte("\r"))
	if !s.open || s.pk.cmdAsk || s.pk.confirm != "" || s.pk.menu != nil {
		t.Fatalf("Enter did not open the switch list: open %v cmdAsk %v", s.open, s.pk.cmdAsk)
	}
	if !s.atPrompt {
		t.Error("the blocked prompt left aggy thinking a turn runs")
	}
	s.pickerInput([]byte{esc}) // the list's Esc: back to the CLI
	s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})

	// The second line, 48 s later: a Backspace, then the command in full.
	for _, k := range append(append([]string{kBS}, strings.Split("/switch codex", "")...), "\r") {
		s.send([]byte(k))
	}
	c, _ = sessioncmd.Parse(adapter.Claude, "/switch codex")
	wantAsked(t, s, s.hookCommand(c, "/switch codex"), false)
	pastGuard(s)
	s.pickerInput([]byte{0x1c}) // the switch key says yes too
	if s.sw.to != adapter.Codex || !s.sw.keyed || s.sw.hooked {
		t.Fatalf("switch %q keyed %v hooked %v, want codex as typed", s.sw.to, s.sw.keyed, s.sw.hooked)
	}
	if s.sw.cleared {
		// aggy typed no Ctrl+U for it: the blocked prompt's composer is taken as
		// empty, unverified, so the status line still asks before /exit.
		t.Error("a hook's question's yes skips the status line")
	}
	if got := sentTo(s); strings.Contains(got, "\x15") {
		t.Errorf("a Ctrl+U went to the CLI: %q", got)
	}
	recs := records()
	asked := recAttrs(t, recs, diag.EvCommandAsked)
	if len(asked) != 2 || asked[0]["why"] != "hook_no_enter,keys_not_a_pick" || asked[0]["pick"] != "menu_key_first" ||
		asked[0]["command"] != "switch" || asked[1]["pick"] != "backspace" {
		t.Errorf("question records %v", asked)
	}
	ans := recAttrs(t, recs, diag.EvCommandAnswered)
	if len(ans) != 2 || ans[0]["answer"] != "yes" || ans[1]["answer"] != "yes" {
		t.Errorf("answer records %v", ans)
	}
	if p := recAttrs(t, recs, diag.EvCommandPassed); len(p) != 0 {
		t.Errorf("records of commands passed to the CLI: %v", p)
	}
}

// TestHookAskKeys (T6982): at the question, Enter, a kitty Enter, or the switch
// key runs the command, but not as the question appears (type-ahead); Esc drops
// it, once no longer key follows; any other key drops it and reaches the CLI.
// Nothing reaches the CLI for the command itself.
func TestHookAskKeys(t *testing.T) {
	for _, c := range []struct {
		name   string
		keys   []string
		soon   bool // the keys come as the question appears
		ran    bool
		answer string // the answer record's, "" for none
		sent   string // what reached the CLI after the question
	}{
		{"Enter", []string{"\r"}, false, true, "yes", ""},
		{"kitty Enter", []string{"\x1b[13u"}, false, true, "yes", ""},
		{"the switch key", []string{"\x1c"}, false, true, "yes", ""},
		{"Enter as it appears", []string{"\r"}, true, false, "", ""},
		{"Esc", []string{"\x1b", "esc"}, false, false, "esc", ""},
		{"Esc that starts a key", []string{"\x1b", "[A"}, false, false, "typed", "\x1b[A"},
		{"other keys", []string{"hi"}, true, false, "typed", "hi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.Diag = q
			s.input = s.send
			sendKeys(s, []string{"/", kDown})
			before := sentTo(s)
			s.toChild = newByteQueue(maxQueued)
			wantAsked(t, s, hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/inbox"}), s.atPrompt)
			if !c.soon {
				pastGuard(s)
			}
			for _, k := range c.keys {
				if k == "esc" {
					s.promptEsc() // escTimeout passed with nothing after the Esc
					continue
				}
				s.pickerInput([]byte(k))
			}
			ran := s.pk.page != nil
			if ran != c.ran {
				t.Fatalf("ran %v, want %v (before: %q)", ran, c.ran, before)
			}
			if !c.ran && c.answer != "" && (s.open || s.hookAsk != nil) {
				t.Fatal("the question stayed open")
			}
			if c.answer == "" && (!s.open || s.hookAsk == nil) {
				t.Fatal("a key typed as the question appeared answered it")
			}
			ans := recAttrs(t, records(), diag.EvCommandAnswered)
			if c.answer == "" && len(ans) != 0 || c.answer != "" && (len(ans) != 1 || ans[0]["answer"] != c.answer) {
				t.Errorf("answer records %v, want %q", ans, c.answer)
			}
			if got := sentTo(s); got != c.sent {
				t.Errorf("the CLI got %q, want %q", got, c.sent)
			}
		})
	}
}

// TestHookAskLimit (T6982): a process in the CLI's tree can raise one question
// at a time, and once hookAskMax went unanswered within hookAskWindow, the next
// command is refused as before the question, the bottom row says so once, and
// the refusal is recorded with its code; a minute later aggy asks again.
func TestHookAskLimit(t *testing.T) {
	q, records := diagHome(t)
	s, _ := viewSess(t, adapter.Claude, "")
	s.o.Diag = q
	s.input = s.send
	forge := func() error { return hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/inbox"}) }
	for i := range hookAskMax {
		s.hookedEnter = time.Time{} // each forged with no Enter of its own
		wantAsked(t, s, forge(), s.atPrompt)
		if i == 0 {
			// One at a time: another while it is open is refused.
			if err := forge(); codeOf(err, diag.Code{}) != diag.CodeCommandNotTaken {
				t.Fatalf("a second question while one is open: err %v", err)
			}
		}
		s.pickerInput([]byte{esc})
		s.promptEsc()
	}
	s.hookedEnter = time.Time{}
	err := forge()
	if codeOf(err, diag.Code{}) != diag.CodeCommandAskLimit || s.open {
		t.Fatalf("err %v open %v, want %s and no question", err, s.open, diag.CodeCommandAskLimit)
	}
	if p := recAttrs(t, records(), diag.EvCommandPassed); len(p) != 1 || p[0]["via"] != "hook" {
		t.Errorf("records of the refusal %v", p)
	}
	s.askDrops[0] = time.Now().Add(-hookAskWindow)
	s.hookedEnter = time.Time{}
	wantAsked(t, s, forge(), s.atPrompt)
}

// TestHookAskStatusLine (T6982): the question is one status line that names the
// command and its keys; a long command is cut, never the keys.
func TestHookAskStatusLine(t *testing.T) {
	p := &picker{key: 0x1c}
	p.askCommand("/switch codex", adapter.Claude)
	if got := p.statusLine(80); got != `aggy: /switch codex from claude's prompt? Enter or Ctrl+\ runs it, Esc drops it` {
		t.Errorf("line %q", got)
	}
	p.askCommand("/aggy diag "+strings.Repeat("a", 60), adapter.Claude)
	got := p.statusLine(80)
	if len(got) > 79 || !strings.HasSuffix(got, "Esc drops it") || !strings.Contains(got, "...") {
		t.Errorf("line %q (%d)", got, len(got))
	}
	var b bytes.Buffer
	p.draw(&b, false)
	if !strings.Contains(b.String(), "aggy did not see it typed") {
		t.Errorf("the full form %q", b.String())
	}
}
