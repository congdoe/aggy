//go:build unix

package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
	"github.com/congdoe/aggy/internal/store"
)

// checkedClaude is the Claude version the live checks of its composer ran on
// (adapter.liveChecks).
const checkedClaude = "2.1.322"

// wantAsked checks that a command from a prompt hook the keys did not vouch for
// was blocked (errAsked: aggy hook blocks the prompt, so no model turn) and
// asked about: the question open, nothing run or switched, atPrompt as it was.
func wantAsked(t *testing.T, s *session, err error, atPrompt bool) {
	t.Helper()
	if !errors.Is(err, errAsked) {
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

// wantRefused checks that aggy ran nothing for a command from a prompt hook
// (code) and asked nothing, yet blocked its prompt (T6982: a blockedErr, so it
// never reaches the model): nothing open or switched, atPrompt as it was.
func wantRefused(t *testing.T, s *session, err error, code diag.Code, atPrompt bool) {
	t.Helper()
	if codeOf(err, diag.Code{}) != code {
		t.Fatalf("err %v, want %s", err, code)
	}
	var be *blockedErr
	if !errors.As(err, &be) || be.note == "" {
		t.Fatalf("err %v: the prompt was not blocked with a note", err)
	}
	if s.open || s.hookAsk != nil || s.sw.to != "" {
		t.Fatalf("refused, yet open %v asked %v switch %q", s.open, s.hookAsk != nil, s.sw.to)
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

// forge sends line as a prompt hook's command, with no Enter of the person's.
func forge(s *session, line string) error {
	s.hookedEnter = time.Time{}
	return hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: line})
}

// TestOwnerRunReplay (T6982): the owner's two lines in Apple Terminal, replayed.
// First, at launch, a Down on the empty prompt, then /switch and Enter; then,
// after that turn's Stop, a Backspace and /switch codex typed in full. Before
// T6982 the stray key spoiled each line for the key catch and the hook's
// command went to the model. On Claude at the checked version both keys change
// nothing on the empty composer (live), so the key catch takes each at once:
// the list before the first turn end, then a switch in one step.
func TestOwnerRunReplay(t *testing.T) {
	noSettleGap(t)
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Claude)
	s.o.Diag, s.o.VendorVersion = q, checkedClaude
	s.input = s.send
	s.turnRan = false // a fresh launch: no Stop yet
	for _, k := range append(append([]string{kDown}, strings.Split("/switch", "")...), "\r") {
		s.send([]byte(k))
	}
	if !s.open || s.pk.cmdAsk || s.pk.confirm != "" || s.pk.menu != nil {
		t.Fatalf("the key catch did not open the switch list: open %v cmdAsk %v", s.open, s.pk.cmdAsk)
	}
	if got := sentTo(s); got != kDown+"/switch\x15" {
		t.Errorf("the CLI got %q, want the Down, the line, and Ctrl+U for its Enter", got)
	}
	s.toChild = newByteQueue(maxQueued)
	s.pickerInput([]byte{esc}) // the list's Esc: back to the CLI
	s.hookEvent(hookEvent{name: "Stop", sessionID: childSID, chain: ownChain})

	// The second line, 48 s later: a Backspace, then the command in full.
	for _, k := range append(append([]string{kBS}, strings.Split("/switch codex", "")...), "\r") {
		s.send([]byte(k))
	}
	if s.sw.to != adapter.Codex || !s.sw.keyed || !s.sw.cleared || s.sw.hooked {
		t.Fatalf("switch %q keyed %v cleared %v hooked %v, want codex caught from the keys", s.sw.to, s.sw.keyed, s.sw.cleared, s.sw.hooked)
	}
	if got := sentTo(s); got != kBS+"/switch codex\x15" {
		t.Errorf("the CLI got %q, want the Backspace, the line, and Ctrl+U for its Enter", got)
	}
	recs := records()
	if caught := recAttrs(t, recs, diag.EvCommandCaught); len(caught) != 2 || caught[0]["via"] != "keys" || caught[1]["via"] != "keys" {
		t.Errorf("caught records %v", caught)
	}
	if p := recAttrs(t, recs, diag.EvCommandPassed); len(p) != 0 {
		t.Errorf("records of commands passed to the CLI: %v", p)
	}
}

// TestOwnerRunUnchecked (T6982): the owner's second line on a Claude version
// with no live check of Backspace: the key catch loses it, and the hook's
// command, which the person typed in full (typedMatch), is blocked and run at
// once as typed (#146 review, third round); with no live fact for a blocked
// prompt at this version, the switch asks on its status line.
func TestOwnerRunUnchecked(t *testing.T) {
	noSettleGap(t)
	q, records := diagHome(t)
	s := cmdSess(t, adapter.Claude)
	s.o.Diag, s.o.VendorVersion = q, "2.1.330"
	s.input = s.send
	for _, k := range append(append([]string{kBS}, strings.Split("/switch codex", "")...), "\r") {
		s.send([]byte(k))
	}
	c, _ := sessioncmd.Parse(adapter.Claude, "/switch codex")
	if err := s.hookCommand(c, "/switch codex"); err != nil || s.open {
		t.Fatalf("err %v open %v: want it run at once", err, s.open)
	}
	if s.sw.to != adapter.Codex || !s.sw.keyed || s.sw.hooked {
		t.Fatalf("switch %q keyed %v hooked %v, want codex as typed", s.sw.to, s.sw.keyed, s.sw.hooked)
	}
	if s.sw.cleared {
		t.Error("one step at a version with no live fact for a blocked prompt")
	}
	if got := sentTo(s); strings.Contains(got, "\x15") {
		t.Errorf("a Ctrl+U went to the CLI: %q", got)
	}
	recs := records()
	if caught := recAttrs(t, recs, diag.EvCommandCaught); len(caught) != 1 || caught[0]["via"] != "hook" || caught[0]["typed"] != true {
		t.Errorf("caught records %v", caught)
	}
	if asked := recAttrs(t, recs, diag.EvCommandAsked); len(asked) != 0 {
		t.Errorf("question records %v", asked)
	}
}

// TestEmptyComposerKeys (#146 review): a key a live check shows changing
// nothing on the empty composer leaves a command typed next known whole: on
// Claude at the checked version, a Down. Up recalls history, the others have
// no check, another version has none, and after text a Down moves in the menu,
// so each of those still spoils the line. An escape sequence never counts as
// composer text.
func TestEmptyComposerKeys(t *testing.T) {
	for _, c := range []struct {
		name    string
		version string
		keys    []string
		caught  bool
	}{
		{"Down first", checkedClaude, []string{kDown}, true},
		{"Down in application mode", checkedClaude, []string{"\x1bOB"}, false}, // not the form checked
		{"Down twice", checkedClaude, []string{kDown, kDown}, true},
		{"another version", "2.1.330", []string{kDown}, false},
		{"Up first", checkedClaude, []string{kUp}, false},
		// Live at 2.1.322 (T6982): these change nothing on the empty composer.
		{"Backspace first", checkedClaude, []string{kBS}, true},
		{"Ctrl+U first", checkedClaude, []string{kCtrlU}, true},
		{"Right first", checkedClaude, []string{"\x1b[C"}, true},
		{"Home first", checkedClaude, []string{"\x1b[H"}, true},
		{"End first", checkedClaude, []string{"\x1b[F"}, true},
		{"Delete first", checkedClaude, []string{"\x1b[3~"}, true},
		{"Backspace, another version", "2.1.330", []string{kBS}, false},
		// Left opens another view in Claude (live): never harmless, and
		// an Esc alone does not end it before a turn end does.
		{"Left first", checkedClaude, []string{kLeft}, false},
		{"Left, then Esc", checkedClaude, []string{kLeft, kEsc}, false},
		{"Ctrl+R first: history search", checkedClaude, []string{"\x12"}, false},
		{"an @ erased first", checkedClaude, []string{"@", kBS}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := cmdSess(t, adapter.Claude)
			s.o.VendorVersion = c.version
			for _, k := range c.keys {
				s.send([]byte(k))
			}
			typeLine(s, "/switch grok")
			if got := caught(sentTo(s), "/switch grok"); got != c.caught {
				t.Errorf("caught %v, want %v", got, c.caught)
			}
		})
	}
	t.Run("a sequence counts no text", func(t *testing.T) {
		s := cmdSess(t, adapter.Claude)
		s.send([]byte(kLeft))
		if s.compLen != 0 || len(s.cmdLine) != 0 {
			t.Errorf("compLen %d cmdLine %q after a Left on the empty composer", s.compLen, s.cmdLine)
		}
	})
}

// TestHookAskKeys (T6982, #146 review): at the question only the switch key
// runs the command, and not as the question appears (type-ahead). Enter, which
// may be meant for the CLI, drops it and goes there, as any other key; Esc
// drops it once no longer key follows.
func TestHookAskKeys(t *testing.T) {
	for _, c := range []struct {
		name   string
		keys   []string
		soon   bool // the keys come as the question appears
		ran    bool
		answer string // the answer record's, "" for none
		sent   string // what reached the CLI after the question
	}{
		{"the switch key", []string{"\x1c"}, false, true, "yes", ""},
		{"the kitty switch key", []string{"\x1b[92;5u"}, false, true, "yes", ""},
		{"the switch key as it appears", []string{"\x1c"}, true, false, "", ""},
		{"Enter", []string{"\r"}, false, false, "typed", "\r"},
		{"kitty Enter", []string{"\x1b[13u"}, false, false, "typed", "\x1b[13u"},
		{"Esc", []string{"\x1b", "esc"}, false, false, "esc", ""},
		{"Esc that starts a key", []string{"\x1b", "[A"}, false, false, "typed", "\x1b[A"},
		{"other keys", []string{"hi"}, true, false, "typed", "hi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.Diag = q
			s.input = s.send
			wantAsked(t, s, forge(s, "/inbox"), true)
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
			if ran := s.pk.page != nil; ran != c.ran {
				t.Fatalf("ran %v, want %v", ran, c.ran)
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

// TestHookAskRefused (#146 review): no question opens where the CLI may be busy
// or a key meant for it could land on the question: a turn that may be
// running (a forged command just after the person's own message), a hold, an
// open ask, unsent text; nor one whose command would not show whole. Each is
// refused as before the question, recorded with why.
func TestHookAskRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(s *session)
		line  string
		why   string
	}{
		{"after the person's message", func(s *session) { typeKeys(s, "fix the bug"); s.send([]byte("\r")) }, "/switch grok", "turn_may_run"},
		{"after the person's /review", func(s *session) { typeKeys(s, "/review"); s.send([]byte("\r")) }, "/switch grok", "turn_may_run"},
		{"a hold", func(s *session) { s.held = true }, "/switch grok", "held"},
		{"unsent text", func(s *session) { typeKeys(s, "draft") }, "/inbox", "unsent_text"},
		{"too long to show", func(s *session) {}, "/aggy diag " + strings.Repeat("a", 64), "too_long_to_ask"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, records := diagHome(t)
			s, _ := viewSess(t, adapter.Claude, "")
			s.o.Diag = q
			c.setup(s)
			atPrompt := s.atPrompt
			wantRefused(t, s, forge(s, c.line), diag.CodeCommandNoEnter, atPrompt)
			p := recAttrs(t, records(), diag.EvCommandBlocked)
			if len(p) == 0 || !strings.HasSuffix(p[len(p)-1]["why"].(string), ","+c.why) {
				t.Errorf("records %v, want why ending %s", p, c.why)
			}
		})
	}
}

// TestForgedYesKeepsState (#146 review H1): a yes to a command the keys did not
// spell runs it as a hook's: the composer and atPrompt stay as they were (here
// a composer in doubt), the switch is not keyed, and its status line still
// asks before anything is typed. A yes to one the keys spelled records the
// blocked prompt: the composer empty, and the switch keyed.
func TestForgedYesKeepsState(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	s.lenKnown, s.unsettled = false, "enter_not_at_prompt" // a turn end that settled nothing
	wantAsked(t, s, forge(s, "/switch grok"), true)
	pastGuard(s)
	s.pickerInput([]byte{0x1c})
	if s.lenKnown || !s.doubt() {
		t.Error("a forged command's yes took the composer as empty")
	}
	if s.sw.to != adapter.Grok || s.sw.keyed || !s.sw.hooked {
		t.Fatalf("switch %q keyed %v hooked %v, want a hook's switch", s.sw.to, s.sw.keyed, s.sw.hooked)
	}
	s.lastIn = time.Now().Add(-time.Minute)
	s.lastOut.Store(time.Now().Add(-time.Minute).UnixNano())
	s.offerSwitch()
	if strings.Contains(sentTo(s), "/exit") {
		t.Fatal("the exit command went with no status line")
	}

	// The keys spelled it: the person's own /switch grok, the key catch lost.
	s2, _ := viewSess(t, adapter.Claude, "")
	s2.input = s2.send
	s2.send([]byte(kBS))
	s2.send([]byte("/switch grok\r")) // with its Enter in one read: no typedMatch
	c, _ := sessioncmd.Parse(adapter.Claude, "/switch grok")
	wantAsked(t, s2, s2.hookCommand(c, "/switch grok"), false)
	pastGuard(s2)
	s2.pickerInput([]byte{0x1c})
	if !s2.lenKnown || !s2.atPrompt || !s2.sw.keyed {
		t.Errorf("lenKnown %v atPrompt %v keyed %v: the spelled command's blocked prompt", s2.lenKnown, s2.atPrompt, s2.sw.keyed)
	}
}

// TestHookAskBeforeTurnEnd (#146 review M3): before the first turn end a yes to
// a hook's /switch only opens the list, as the hook's switch taken at once does.
func TestHookAskBeforeTurnEnd(t *testing.T) {
	s := cmdSess(t, adapter.Claude)
	s.turnRan, s.atPrompt, s.inputSinceTurn = false, true, false
	wantAsked(t, s, forge(s, "/switch grok"), true)
	pastGuard(s)
	s.pickerInput([]byte{0x1c})
	if s.sw.to != "" || !s.open || s.pk.cmdAsk || s.pk.confirm != "" {
		t.Fatalf("switch %q open %v: want the list, no switch queued", s.sw.to, s.open)
	}
}

// TestHookAskFillWaits (#146 review): a yes to a hook's /borrow <kind> types
// nothing into the composer once a turn may be running.
func TestHookAskFillWaits(t *testing.T) {
	s, _ := viewSess(t, adapter.Claude, "")
	wantAsked(t, s, forge(s, "/borrow res"), true)
	s.atPrompt = false // a turn started meanwhile
	pastGuard(s)
	s.pickerInput([]byte{0x1c})
	if got := sentTo(s); got != "" {
		t.Errorf("typed into a running turn: %q", got)
	}
	if !s.open || !s.pk.note {
		t.Error("nothing said why it was not typed")
	}
}

// TestHookAskLimit (T6982): a process in the CLI's tree can raise one question
// at a time, and once hookAskMax went unanswered within hookAskWindow, the next
// command is refused as before the question, the bottom row says so once, and
// the refusal is recorded with its code; a minute later aggy asks again. A
// question another screen closed does not count (#146 review).
func TestHookAskLimit(t *testing.T) {
	q, records := diagHome(t)
	s, _ := viewSess(t, adapter.Claude, "")
	s.o.Diag = q
	s.input = s.send
	wantAsked(t, s, forge(s, "/inbox"), true)
	s.pk.tell("a note that takes aggy's screen")
	s.openPicker()
	s.closePicker()
	if len(s.askDrops) != 0 {
		t.Fatalf("a question another screen closed counted: %v", s.askDrops)
	}
	for i := range hookAskMax {
		wantAsked(t, s, forge(s, "/inbox"), true)
		if i == 0 {
			// One at a time: another while it is open is refused.
			if err := forge(s, "/inbox"); codeOf(err, diag.Code{}) != diag.CodeCommandNotTaken {
				t.Fatalf("a second question while one is open: err %v", err)
			}
		}
		s.pickerInput([]byte{esc})
		s.promptEsc()
	}
	wantRefused(t, s, forge(s, "/inbox"), diag.CodeCommandAskLimit, true)
	// The second while one was open, then the one over the limit.
	if p := recAttrs(t, records(), diag.EvCommandBlocked); len(p) != 2 || p[1]["via"] != "hook" || !strings.HasSuffix(p[1]["why"].(string), ",ask_limit") {
		t.Errorf("records of the refusal %v", p)
	}
	s.askDrops[0] = time.Now().Add(-hookAskWindow)
	wantAsked(t, s, forge(s, "/inbox"), true)
}

// TestHookAskStatusLine (T6982, #146 review M1): the question names the command
// in canonical form, its words one space apart as Parse reads them, so runs of
// spaces cannot push what a yes runs out of view; it is never cut, and a
// window too narrow for it says to widen it, where the switch key does nothing.
func TestHookAskStatusLine(t *testing.T) {
	p := &picker{key: 0x1c}
	p.askCommand("/switch codex", adapter.Claude)
	if got := p.statusLine(80); got != `aggy: /switch codex from claude's prompt? Ctrl+\ runs it, Esc drops it` {
		t.Errorf("line %q", got)
	}
	if got := p.statusLine(40); strings.Contains(got, "/switch") || !strings.Contains(got, "widen") {
		t.Errorf("narrow line %q", got)
	}
	var b bytes.Buffer
	p.draw(&b, false)
	if !strings.Contains(b.String(), "aggy did not see it typed") {
		t.Errorf("the full form %q", b.String())
	}

	s, _ := viewSess(t, adapter.Claude, "")
	wantAsked(t, s, forge(s, "/switch"+strings.Repeat(" ", 200)+"codex   --new"), true)
	if got := s.pk.statusLine(80); !strings.Contains(got, "/switch codex --new from") {
		t.Errorf("line %q, want the canonical command", got)
	}
	s.t = &tty{fd: -1}
	pastGuard(s)
	s.hookAsk.line = "/aggy diag " + strings.Repeat("a", 64) // as if the window shrank
	s.pickerInput([]byte{0x1c})
	if s.hookAsk == nil || s.sw.to != "" {
		t.Error("the switch key ran a command the line could not show")
	}
}
