//go:build unix

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// Commands inside a session (T6908, docs/design.md section 5): a command typed
// in full at the prompt is caught from the keystrokes. aggy holds back its
// Enter, clears the composer with Ctrl+U, and acts, so nothing reaches the CLI
// and no model turn is spent. That Ctrl+U clears the composer line is
// unverified on every CLI (the composer tracker assumes it already, send).
//
// Only a line the composer tracker knows whole is taken: typed since an Enter
// that left the composer empty, as printable keys, with no paste, no cursor
// key, no edit, no history recall, no newline, and no Esc; and only on an Enter
// that comes alone in its read, as a person's does. Anything else, a paste
// that ends in an Enter included, goes to the CLI as typed, where a miss is
// harmless: the CLI refuses an unknown command, or a skill tells the model to
// change nothing.

// clearsLine reports whether a live check shows Ctrl+U emptying the composer of
// v (adapter.ClearLineChecked): only then does a switch typed in full skip the
// status line (offerSwitch). A variable for tests.
var clearsLine = adapter.ClearLineChecked

// catchCommand takes a read that is one plain Enter (raw, as it came, and
// typed, as legacyKeys gave it) ending a command the tracker knows whole. It
// reports whether it took it: then nothing of the read reaches the CLI.
func (s *session) catchCommand(raw, typed []byte) bool {
	if string(typed) != "\r" || !plainEnter(raw) || !s.lineKnown() {
		return false
	}
	c, ok := sessioncmd.Parse(s.o.Vendor, string(s.cmdLine))
	if !ok || !s.takes(c) {
		return false
	}
	if s.altBlocks() {
		return false
	}
	clear := []byte{ctrlU}
	if raw[0] == esc {
		// The terminal reports keys as CSI u: Ctrl+U the same way (unverified
		// that each CLI takes it so; its legacy byte may also do).
		// TODO(T6954): a modifyOtherKeys Enter (CSI 27;1;13~) gets the CSI u
		// form too, which a terminal in that mode would not send itself.
		clear = []byte("\x1b[117;5u")
		s.enterRelease = true // the release that follows is the held Enter's
	}
	s.toChild.push(clear)
	s.lastIn = time.Now()
	s.inputSinceTurn = s.lineInput // the line and its Ctrl+U together typed nothing
	s.typedSince = false
	s.emptyComposer()
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": c.Name, "via": "keys"}})
	// Only the byte form of Ctrl+U has a live check (adapter.ClearLineChecked),
	// so a switch this clears with the CSI u form still asks (pickSwitch).
	s.byteClear = raw[0] != esc
	s.runCommand(c, true)
	s.byteClear = false
	return true
}

// lineKnown reports whether the composer holds exactly cmdLine: typed since an
// Enter that sent what was there (or since launch), as printable keys only.
func (s *session) lineKnown() bool { return len(s.lineDoubts()) == 0 }

// lineDoubts names, in a fixed order, each reason the tracker does not know
// the composer holds exactly cmdLine; none when it does. The names are what a
// miss record says (noteMiss, docs/logging.md), so each stays a fixed word.
func (s *session) lineDoubts() []string {
	var why []string
	for _, d := range []struct {
		on   bool
		name string
	}{
		{len(s.cmdLine) == 0, "no_line"},
		{s.cmdBad, "not_plain_keys"},
		{!s.lenKnown && !s.cmdFresh, "len_unknown"},
		{s.draftBlind, "draft_blind"},
		{s.draftPaste, "draft_paste"},
		{s.inPaste, "in_paste"},
		{s.pastePop, "paste_popup"},
		{s.cleared, "cleared"},
		{s.escDoubt, "esc_doubt"},
		{s.escEmpty, "esc_empty"},
		{s.escTwice, "esc_twice"},
		{s.moved, "moved"},
		{s.askOpen(), "ask_open"},
		{s.exitGap, "exit_gap"},
		{s.sideView, "side_view"},
		{s.modeLine, "mode_line"},
		{s.compLen != len(s.cmdLine), "comp_len"},
		{s.draftLen != len(s.cmdLine), "draft_len"},
	} {
		if d.on {
			why = append(why, d.name)
		}
	}
	return why
}

// altBlocks reports whether the alternate screen keeps a command from being
// caught: Claude and Grok render inline, so there the keys go to an editor or
// a pager the CLI opened, where / searches.
func (s *session) altBlocks() bool {
	if s.o.Vendor != adapter.Claude && s.o.Vendor != adapter.Grok {
		return false
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.alt.on
}

// plainEnter reports whether raw is one press of Enter with no modifier: the
// byte, or its kitty or modifyOtherKeys report (Caps and Num Lock allowed). A
// Shift+Enter or an Alt+Enter is a newline in some composers.
func plainEnter(raw []byte) bool {
	if string(raw) == "\r" {
		return true
	}
	r, ok := parseCSIu(raw)
	return ok && r.n == len(raw) && (r.code == 13 || r.code == kittyKeypadCR) && (r.mods-1)&^modLocks == 0 && (r.event == 0 || r.event == 1)
}

// enterReleaseOnly reports whether raw is one release of Enter, as kitty's
// report-all-events mode sends after a press.
func enterReleaseOnly(raw []byte) bool {
	r, ok := parseCSIu(raw)
	return ok && r.n == len(raw) && (r.code == 13 || r.code == kittyKeypadCR) && r.event == 3
}

// emptyComposer records that the composer is empty, as after a clean send.
func (s *session) emptyComposer() {
	s.compLen, s.lenKnown, s.popup, s.cleared, s.hadAt, s.moved, s.pastePop = 0, true, false, false, false, false, false
	s.slashOnly, s.slashEntered, s.slashLine = false, false, false
	s.draftLen, s.draftBlind, s.draftHigh = 0, false, false
	s.cmdLine, s.cmdBad = s.cmdLine[:0], false
	s.pick = menuPick{}
	s.toldDirty, s.cmdFresh = false, false
	s.resetTyped()
	s.updateDirty()
}

// takes reports whether this session acts on c. Each kind is taken only where
// it has something to act with.
func (s *session) takes(c sessioncmd.Command) bool {
	if name, _, _ := strings.Cut(c.Name, " "); s.o.CommandOff != nil && s.o.CommandOff(name) {
		return false
	}
	switch c.Kind {
	case sessioncmd.Picker, sessioncmd.Switch:
		return true
	case sessioncmd.Pick:
		return c.List == "switch" || s.o.Aggy != nil
	case sessioncmd.Run, sessioncmd.AggyList, sessioncmd.BorrowList, sessioncmd.ToolList, sessioncmd.Fill:
		return s.o.Aggy != nil
	case sessioncmd.TrustList:
		return s.o.TrustList != nil && s.o.TrustAnswer != nil
	}
	return false
}

// runCommand acts on a command, from the keyboard (keyed) or a hook.
func (s *session) runCommand(c sessioncmd.Command, keyed bool) {
	switch c.Kind {
	case sessioncmd.Picker:
		if s.open {
			s.closePicker()
		}
		s.openList()
	case sessioncmd.Switch:
		if s.open {
			s.closePicker()
		}
		// A switch a hook sent is not the person's: a fix it needs is told
		// in a note, not opened as a page (#133 review).
		s.agentPick = !keyed
		s.pickSwitch(Choice{Vendor: c.Vendor, Brief: c.Brief, New: c.New, Model: c.Model}, keyed)
		s.agentPick = false
	case sessioncmd.AggyList:
		s.showMenu(&menu{title: "commands", items: s.aggyItems(), filterable: true})
	case sessioncmd.BorrowList:
		s.showMenu(s.borrowMenu(c, ""))
	case sessioncmd.Pick:
		s.openPick(c)
	case sessioncmd.Fill:
		if s.open {
			s.closeView()
		}
		s.fillComposer(c.Line)
	case sessioncmd.ToolList:
		s.startRun(c, nil)
	case sessioncmd.Run:
		if s.toolByPrefix(c) {
			return
		}
		s.startRun(c, s.cmdArgs(c))
	case sessioncmd.TrustList:
		s.startTrust(c)
	}
}

// promptBlockWait bounds how long after the Enter a prompt hook's command
// may say that Enter started no turn. The hook runs before the model
// (Claude hooks.md, UserPromptSubmit), within aggy hook's few seconds.
const promptBlockWait = 10 * time.Second

// promptRepeat is how soon the same command from a prompt hook counts as the
// same prompt reported again.
const promptRepeat = 2 * time.Second

// promptAnswerWait bounds how long a socket goroutine waits for the main loop
// to answer a prompt-command: under aggy hook's own 3 seconds, so a busy loop
// blocks the prompt (busyBlock) rather than the hook timing out.
const promptAnswerWait = 2 * time.Second

// promptReq is one prompt-command on its way to the main loop: the command,
// where the answer goes, and claimed, which the main loop or a socket
// goroutine that gave up waiting sets first; whoever does decides.
type promptReq struct {
	c       sessioncmd.Command
	line    string
	answer  chan error
	claimed *atomic.Bool
}

// onPromptCommand takes a command a CLI's UserPromptSubmit hook reported
// (T6910), on a connection's goroutine: the line is parsed again here, only
// from the vendor on screen, and only where aggy installs that hook (Claude and
// Grok). The main loop then decides (hookCommand). A nil answer, errAsked, or a
// blockedErr makes aggy hook block the prompt; only a command turned off in the
// person's config, or one this session has nothing to run with (takes), and a
// request that is not one of aggy's commands, let it through.
func (s *session) onPromptCommand(r Request) error {
	on := s.o.Vendor
	if r.Vendor != string(on) {
		return coded(diag.CodeWrongVendor, fmt.Errorf("a command from %s's hook, but %s is on screen", r.Vendor, on))
	}
	if on != adapter.Claude && on != adapter.Grok {
		return coded(diag.CodeCommandNotTaken, fmt.Errorf("aggy installs no prompt hook for %s", on))
	}
	c, ok := sessioncmd.Parse(on, r.Command)
	off := false
	if name, _, _ := strings.Cut(c.Name, " "); ok && s.o.CommandOff != nil {
		off = s.o.CommandOff(name)
	}
	// A keyboard-only command goes on to be blocked (hookCommand), unless off.
	if !ok || off || !c.KeyboardOnly && !s.takes(c) {
		return coded(diag.CodeCommandNotTaken, errors.New("not a command aggy takes from a hook"))
	}
	p := promptReq{c: c, line: r.Command, answer: make(chan error, 1), claimed: new(atomic.Bool)}
	select {
	case s.prompts <- p:
	default:
		return busyBlock(c, on, s.o.SwitchKey, "another command from the prompt is still waiting")
	}
	t := time.NewTimer(promptAnswerWait)
	defer t.Stop()
	select {
	case err := <-p.answer:
		return err
	case <-t.C:
	}
	if p.claimed.CompareAndSwap(false, true) {
		return busyBlock(c, on, s.o.SwitchKey, "aggy was busy")
	}
	return <-p.answer // the main loop claimed it first and answers now
}

// blockedErr is hookCommand's answer when it ran nothing for a command a
// prompt hook reported but the prompt is blocked all the same (T6982): the
// hook's answer goes only to the process that called it, so a forged call
// gains nothing from a block, and a real one must never reach the model. note
// says why in plain words, and the keyboard way through; aggy hook shows it as
// the block's reason. recorded: the supervisor wrote its record already.
type blockedErr struct {
	err      error
	note     string
	recorded bool
}

func (e *blockedErr) Error() string { return e.err.Error() }
func (e *blockedErr) Unwrap() error { return e.err }

// busyBlock is a blockedErr for a command the main loop never saw.
// key is Options.SwitchKey, which the session's goroutines may read.
func busyBlock(c sessioncmd.Command, on adapter.Vendor, key byte, why string) error {
	if key == 0 {
		key = 0x1c
	}
	return &blockedErr{err: coded(diag.CodeCommandNotTaken, errors.New(why)),
		note: fmt.Sprintf("aggy did not run %s%s (%s). %s", sessioncmd.Prefix(on), c.Name, why, wayThrough(c, on, key, false))}
}

// wayThrough is the keyboard's way to what c would have done: the switch key
// for a switch, which no forged request can press and which works whatever
// aggy knows of the prompt; otherwise the command typed in full, which proves
// itself (typedMatch) whatever aggy doubted before the line. After Left with no
// Esc the keys go to Claude's other view, so an Esc comes first (sideNoEsc).
func wayThrough(c sessioncmd.Command, on adapter.Vendor, key byte, sideNoEsc bool) string {
	switch {
	case c.Kind == sessioncmd.Switch || c.Kind == sessioncmd.Picker:
		return keyName(key) + " opens the switch list."
	case sideNoEsc:
		return fmt.Sprintf("Press Esc, then type it in full at %s's prompt.", on)
	}
	return fmt.Sprintf("Type it in full at %s's prompt.", on)
}

// typedMatch reports whether the person typed line (#146 review, third
// round): the printable keys since the previous Enter, with any non-printing
// key or escape sequence before the first of them left out, spell exactly the
// command; no paste, edit, cursor key, or newline came after the first of
// them; the Enter came alone; no key since; within promptBlockWait, or, for an
// Enter that went while a turn may have run (the CLI queues the line, and its
// hook fires when it takes it), any time before another key. A real hook that
// reports exactly that command then proves the composer held exactly that
// line, whatever aggy doubted before it: anything else in the composer would
// have made another prompt. A forger cannot see the keys; guessing right only
// claims what the person typed.
func (s *session) typedMatch(line string) bool {
	switch {
	case s.enterAt.IsZero() || s.afterEnter != 0 || !s.enterTyped || !s.enterAlone,
		s.enterAtPrompt && time.Since(s.enterAt) >= promptBlockWait,
		canonLine(string(s.enterLineTyped)) != canonLine(line):
		return false
	}
	return true
}

// blockHook runs nothing for c, which a prompt hook reported as line, and keeps
// its prompt from the model (blockedErr). why records the rule that failed, as
// fixed words (hookMiss); with none, nothing is recorded here. plain says it to
// the person, on the bottom row (at most hookAskMax a window) and as the
// block's reason; "" says nothing on the bottom row.
//
// When the keys showed the Enter for this very line (spelled), the prompt the
// hook blocked is the one that Enter sent, so the CLI is back at its prompt with
// its composer empty (promptBlocked; on Claude 2.1.322 recorded live). A
// request alone, which any process in the tree can send, changes nothing.
func (s *session) blockHook(c sessioncmd.Command, line string, code diag.Code, why []string, plain string) error {
	if why != nil {
		s.hookMiss(c, why)
	}
	if !s.enterAt.IsZero() && !s.hookedEnter.Equal(s.enterAt) && s.spelled(canonLine(line)) {
		s.heldMiss = nil
		s.hookedEnter = s.enterAt // a second report of this Enter is blocked too, silently
		s.promptBlocked()
	}
	way := wayThrough(c, s.o.Vendor, s.pk.key, s.sideView && !s.sideEsc)
	note := fmt.Sprintf("aggy did not run %s%s. %s", sessioncmd.Prefix(s.o.Vendor), c.Name, way)
	if plain != "" {
		note = fmt.Sprintf("aggy did not run %s%s (%s). %s", sessioncmd.Prefix(s.o.Vendor), c.Name, plain, way)
		// TODO(T6954): a process in the tree can forge requests that draw up to
		// hookAskMax of these notes a minute.
		now := time.Now()
		s.blockNotes = slices.DeleteFunc(s.blockNotes, func(t time.Time) bool { return now.Sub(t) >= hookAskWindow })
		if len(s.blockNotes) < hookAskMax && !s.open {
			s.blockNotes = append(s.blockNotes, now)
			s.waitNote("aggy: " + strings.TrimPrefix(note, "aggy "))
		}
	}
	return &blockedErr{err: coded(code, errors.New(note)), note: note, recorded: why != nil}
}

// blockPlain says a reason aggy did not ask about a command in a few words.
func (s *session) blockPlain(not string) string {
	on := s.o.Vendor
	switch not {
	case "held":
		return fmt.Sprintf("%s may be showing a question or an approval", on)
	case "ask_open":
		return "another question of aggy's is open"
	case "unsent_text":
		return fmt.Sprintf("%s's prompt holds unsent text", on)
	case "turn_may_run":
		return fmt.Sprintf("cannot tell if %s is idle", on)
	case "too_long_to_ask":
		return "too long to show on aggy's status line"
	case "no_turn_yet":
		return fmt.Sprintf("%s has not finished a turn yet", on)
	case "ask_limit":
		return "too many questions went unanswered in the last minute"
	case "keyboard_only":
		return "it changes something, so aggy takes it only from the keys"
	case "screen_open":
		return "aggy's own screen is open"
	case "switch_waiting":
		return "a switch is already waiting"
	}
	return "the keys do not vouch for it"
}

// answerPrompt is the main loop's side of onPromptCommand.
func (s *session) answerPrompt(p promptReq) {
	if !p.claimed.CompareAndSwap(false, true) {
		return // its socket goroutine gave up; the prompt went to the CLI
	}
	p.answer <- s.hookCommand(p.c, p.line)
}

// hookCommand acts on a command a prompt hook reported as line, or says why
// not. Any process in the CLI's tree can send one, so only the one that goes
// with a real Enter is taken (R121): an Enter at the prompt, on a line that
// held only a leading / command, with no key since, within promptBlockWait,
// and one command per Enter. And the keys before that Enter must be able to
// have picked line from the CLI's menu (T6975, menuPick and sessioncmd.Picked):
// the person typed at least / and one letter of its name (/sw for /switch),
// then perhaps Up, Down, or Tab, and after a Tab its arguments. Such a command
// is taken at once. Nothing is taken over a list, page, or picker already on
// screen, nor a switch over one waiting. The hook then blocked that prompt, so
// that Enter started no turn: the prompt is where it was, and its line is gone
// from the composer (both CLIs document a block as stopping the prompt; that
// the composer is left empty is unverified, TODO(T6895)). A switch caught this
// way always asks on the status line.
//
// When the keys do not vouch for the command (the tracker lost the line, or a
// forged request names nothing the person typed), the prompt is blocked all the
// same, so it never reaches the model, and aggy asks on its status line
// (askHookCommand, T6982): a key there is the proof a forged request lacks.
func (s *session) hookCommand(c sessioncmd.Command, line string) error {
	// Claude may report one prompt twice, as submitted and as a command's
	// expansion (UserPromptSubmit and UserPromptExpansion, hooks.md; which of
	// them a menu pick sends is unverified). The second blocks too and does
	// nothing more.
	if c.Name == s.hookedName && s.hookedEnter.Equal(s.enterAt) && time.Since(s.enterAt) < promptRepeat {
		return nil
	}
	doubts := s.hookDoubts(line)
	switch {
	case !s.hookedEnter.IsZero() && s.hookedEnter.Equal(s.enterAt):
		// That Enter's command was taken or blocked: this one is a repeat, or a
		// forged request that won the race for it; recorded as a repeat.
		return s.blockHook(c, line, diag.CodeCommandNoEnter, []string{"repeat"}, "")
	case c.KeyboardOnly:
		// It changes something, and any process in the CLI's tree can send this.
		return s.blockHook(c, line, diag.CodeCommandKeyboard, append(doubts, "keyboard_only"), s.blockPlain("keyboard_only"))
	case s.open:
		return s.blockHook(c, line, diag.CodeCommandNotTaken, append(doubts, "screen_open"), s.blockPlain("screen_open"))
	case (c.Kind == sessioncmd.Switch || c.Kind == sessioncmd.Picker) && s.sw.to != "":
		return s.blockHook(c, line, diag.CodeCommandNotTaken, append(doubts, "switch_waiting"), s.blockPlain("switch_waiting"))
	case len(doubts) > 0 && s.typedMatch(line) && (!s.sideView || s.sideEsc) && !s.held && !s.askOpen() && s.hookAsk == nil:
		// The person typed this very line, and the hook reports it: act as on
		// a command typed in full (#146 review, third round). Not while Left
		// may have sent the keys to Claude's other view (no Esc since), nor
		// over a hold or a question of the CLI's.
		return s.actTyped(c)
	case len(doubts) > 0:
		return s.askHookCommand(c, line, doubts)
	}
	s.hookedName, s.hookedEnter = c.Name, s.enterAt
	s.heldMiss = nil // the key catch's miss of this Enter was the hook's catch
	s.promptBlocked()
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": c.Name, "via": "hook"}})
	if c.Kind == sessioncmd.Switch && !s.turnRan {
		// Before a turn end only quitting the CLI completes a switch, and a
		// forged hook must not set that up: the list says it, and a pick there
		// is the person's.
		c = sessioncmd.Command{Kind: sessioncmd.Picker, Name: c.Name}
	}
	s.runCommand(c, false)
	if c.Kind == sessioncmd.Switch && s.sw.to == c.Vendor && !s.sw.typed {
		// A hook can be forged, so the status line asks, and says where the
		// switch came from.
		s.sw.hooked, s.sw.keyed, s.sw.doubtOK = true, false, true
	}
	return nil
}

// actTyped runs c, a command typedMatch shows the person typed and the hook
// blocked. That proves intent, not state (#146 review, fourth round): the
// person asked for c, so no question is needed, but not where the Enter went
// or what the composer held. A forger who saw the line can send the same
// request while the Enter went into Claude's dialog, its other view, or a
// queue behind a running turn, or sent history Up recalled with the line. So
// aggy's view of the CLI stays exactly as it was: c runs at once if it types
// nothing into the CLI; a switch waits as one from a hook does (hooked: the
// status line asks before /exit, once aggy takes the CLI as idle, at a turn
// end or when the person picks the vendor again in the list); a fill is
// refused while a turn may run. Before a turn end a switch opens the list,
// as one caught from the keys does. A line the keys show typed at an idle
// prompt on a composer known empty is the key catch's (catchCommand), which
// types Ctrl+U itself; the hook never sees it.
func (s *session) actTyped(c sessioncmd.Command) error {
	s.hookedName, s.hookedEnter = c.Name, s.enterAt
	s.heldMiss = nil // the key catch's miss of this Enter was the hook's catch
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": c.Name, "via": "hook", "typed": true}})
	if c.Kind == sessioncmd.Switch && !s.turnRan {
		c = sessioncmd.Command{Kind: sessioncmd.Picker, Name: c.Name}
	}
	if c.Kind == sessioncmd.Fill && s.mayBeRunning() {
		s.pk.tell(fmt.Sprintf("aggy did not type %s into %s's prompt: a turn may be running there.", c.Line, s.o.Vendor),
			"Type it yourself once the turn ends.")
		s.openPicker()
		return nil
	}
	s.runCommand(c, false)
	if c.Kind == sessioncmd.Switch && s.sw.to == c.Vendor && !s.sw.typed {
		s.sw.hooked, s.sw.keyed, s.sw.cleared = true, false, false
		if s.turnMayRun() && !s.open {
			s.waitNote(fmt.Sprintf("aggy: switches to %s at %s's turn end. None coming? %s, pick %s.", c.Vendor, s.o.Vendor, keyName(s.pk.key), c.Vendor))
		}
	}
	return nil
}

// enterVouches reports whether the last Enter can be the one that submitted a
// prompt the hook reports now: it went at the prompt, with no key since, within
// promptBlockWait.
func (s *session) enterVouches() bool {
	return s.enterAtPrompt && s.afterEnter == 0 && time.Since(s.enterAt) < promptBlockWait
}

// hookDoubts names why the keys do not vouch for line, a command a prompt hook
// reported, as fixed words: hook_no_enter (no such Enter, or keys before it no
// menu pick could be) or hook_keys_differ (they do not start line), then the
// parts that failed. None when they vouch for it.
func (s *session) hookDoubts(line string) []string {
	var why []string
	switch {
	case !s.enterVouches() || s.enterPick.bad:
		why = append(why, "hook_no_enter")
	case !sessioncmd.Picked(s.o.Vendor, string(s.enterPick.text), s.enterPick.tab, string(s.enterPick.args), line):
		why = append(why, "hook_keys_differ")
	default:
		return nil
	}
	for _, d := range []struct {
		on   bool
		name string
	}{
		{!s.enterAtPrompt, "enter_not_at_prompt"},
		{s.enterPick.bad, "keys_not_a_pick"},
		{s.afterEnter != 0, "keys_after_enter"},
		{time.Since(s.enterAt) >= promptBlockWait, "enter_too_old"},
		{s.held, "held"},
	} {
		if d.on {
			why = append(why, d.name)
		}
	}
	return why
}

// promptBlocked records what a blocked prompt left: the CLI at its prompt, as it
// was, and its composer empty. It started nothing, as a caught command's line
// does not (catchCommand), so a pick it opens is told what the CLI's launch
// state means, not held back as if a turn might run (T6982).
func (s *session) promptBlocked() {
	s.atPrompt = !s.held
	s.inputSinceTurn = s.lineInput
	if s.sideEsc {
		// The hook ran for a prompt from the composer: Claude's other view,
		// which the Esc since Left may have closed, is closed.
		s.sideView, s.sideEsc = false, false
	}
	// A blocked prompt is taken as an empty composer: recorded live for Claude
	// 2.1.322 (docs/verification.md, "Claude Code's Ctrl+U and /exit").
	// TODO(T6975): unverified on Grok, and on other Claude versions; if the CLI
	// puts the text back, an exit command is typed after it.
	s.emptyComposer()
}

// errAsked is hookCommand's answer when it blocked the prompt to ask about the
// command on the status line (askHookCommand): aggy hook blocks the prompt with
// a reason of its own (Response.Type RespAsked).
var errAsked = errors.New("aggy asks about the command on its status line")

// hookQuestion is a command from a prompt hook that aggy asks about
// (askHookCommand): the command, its line in canonical form, the Enter it came
// after and whether the keys vouched that Enter sent that line (spelled), and
// the record of the question.
type hookQuestion struct {
	c       sessioncmd.Command
	line    string
	enterAt time.Time
	spelled bool
	rec     string
}

// hookAskWindow and hookAskMax bound the questions a prompt hook can raise: once
// hookAskMax of them closed unanswered within hookAskWindow, the next command
// is refused, as before the question, and the bottom row says so once a window.
const (
	hookAskWindow = time.Minute
	hookAskMax    = 3
)

// canonLine is line as its command reads: its prefix and its words, one space
// apart. Parse takes words so, so this is what a yes runs; runs of spaces in a
// forged line could push its arguments out of view (#146 review).
func canonLine(line string) string {
	if line == "" {
		return ""
	}
	return line[:1] + strings.Join(strings.Fields(line[1:]), " ")
}

// spelled reports whether the keys say the last Enter sent line: it went at the
// prompt with no key since (enterVouches), on a line that began with / on a
// composer aggy counted empty (enterSlash), whose printable keys spell line's
// words. A forged command after the person's own /review or message is not.
func (s *session) spelled(line string) bool {
	return s.enterVouches() && s.enterSlash && canonLine(string(s.enterLine)) == canonLine(line)
}

// askHookCommand blocks the prompt that carried c, which the keys do not vouch
// for (why), and asks on the status line whether to run it (T6982). The
// person's switch key there is the proof a forged request lacks, so a process
// in the CLI's tree can at worst raise a question: one at a time (hookCommand
// blocks another while aggy's screen is up), and none once hookAskMax went
// unanswered within hookAskWindow. None opens where a key meant for the CLI
// could land on it, or the CLI could be busy: a hold, an open ask, a turn that
// may be running, or unsent text, unless the keys spelled this line (the
// prompt the CLI just sent was this one, now blocked); nor one whose command
// would not show whole on the line, nor, before a turn end, one for anything
// but the switch list. Those run nothing, and their prompt is blocked all the
// same, with a note (blockHook).
func (s *session) askHookCommand(c sessioncmd.Command, line string, why []string) error {
	line = canonLine(line)
	spelled := s.spelled(line)
	not := ""
	switch {
	case s.held:
		not = "held"
	case s.askOpen():
		not = "ask_open"
	case !spelled && s.knownText():
		not = "unsent_text"
	case !spelled && s.mayBeRunning():
		not = "turn_may_run"
	case !s.turnRan && c.Kind != sessioncmd.Switch && c.Kind != sessioncmd.Picker:
		// Before a turn end only the switch list is offered on a question.
		not = "no_turn_yet"
	case !s.pk.commandFits(line, s.o.Vendor, s.cols()):
		// Blocked with a note like the others (since T6982, not passed).
		not = "too_long_to_ask"
	}
	if not != "" {
		if !slices.Contains(why, not) { // hookDoubts may already name held
			why = append(why, not)
		}
		return s.blockHook(c, line, diag.CodeCommandNoEnter, why, s.blockPlain(not))
	}
	now := time.Now()
	s.askDrops = slices.DeleteFunc(s.askDrops, func(t time.Time) bool { return now.Sub(t) >= hookAskWindow })
	if len(s.askDrops) >= hookAskMax {
		return s.blockHook(c, line, diag.CodeCommandAskLimit, append(why, "ask_limit"), s.blockPlain("ask_limit"))
	}
	if spelled {
		s.heldMiss = nil // the key catch's miss of this Enter: the prompt is blocked
	} else {
		s.flushMiss(true)
	}
	s.hookedName, s.hookedEnter = c.Name, s.enterAt
	attrs := s.missAttrs(c.Name, why)
	if s.enterPick.bad && s.enterPick.why != "" {
		attrs["pick"] = s.enterPick.why
	}
	attrs["spelled"] = spelled
	s.hookAsk = &hookQuestion{c: c, line: line, enterAt: s.enterAt, spelled: spelled, rec: s.log(diag.Rec{Event: diag.EvCommandAsked, Attrs: attrs})}
	s.pk.askCommand(line, s.o.Vendor)
	s.openPicker()
	return errAsked
}

// cols is the terminal's width, 80 when it cannot be read.
func (s *session) cols() int {
	if _, c := s.t.size(); c > 1 {
		return c
	}
	return 80
}

// hookAskInput takes a key at the question askHookCommand opened. Only the
// switch key says yes, once confirmGuard has passed, so a key typed as the
// question appeared answers nothing, and an Enter meant for the CLI (an
// approval, a message) never runs a command (#146 review); and only while the
// command shows whole. Esc drops it, once escTimeout shows it began no longer
// key; any other key, Enter among them, drops it and goes to the CLI as typed.
func (s *session) hookAskInput(b []byte) {
	if s.escBy != nil {
		s.escBy = nil
		s.dropHookAsk("typed")
		s.input(append([]byte{esc}, b...))
		return
	}
	switch {
	case isEsc(b):
		s.escBy = time.After(escTimeout)
	case s.pk.isKey(b):
		if time.Since(s.openedAt) < confirmGuard || !s.pk.commandFits(s.hookAsk.line, s.o.Vendor, s.cols()) {
			return // too soon to be meant, or the command does not show: ignored
		}
		s.runHookAsk()
	default:
		s.dropHookAsk("typed")
		s.input(b)
	}
}

// runHookAsk runs the command the person said yes to. Only when the keys
// spelled it (spelled) is it run as one typed in full, keyed, with the blocked
// prompt's state recorded: the CLI at its prompt, its composer empty. Any other
// is run as a hook's: a switch always asks on the status line and waits for the
// full checks, a turn end included, and atPrompt and the composer stay as they
// were, so a forged request answered yes cannot mark a running CLI idle (#146
// review). Before the first turn end a switch only opens the list, as from the
// hook at once; a fill waits for no turn to be running.
func (s *session) runHookAsk() {
	a := s.hookAsk
	s.hookAsk = nil
	s.closePicker()
	s.log(diag.Rec{Event: diag.EvCommandAnswered, Cause: a.rec, Attrs: map[string]any{"command": a.c.Name, "answer": "yes"}})
	spelled := a.spelled && s.enterAt.Equal(a.enterAt) && s.afterEnter == 0
	c := a.c
	if c.Kind == sessioncmd.Fill && !spelled && s.mayBeRunning() {
		s.pk.tell(fmt.Sprintf("aggy did not type %s into %s's prompt: a turn may be running there.", a.line, s.o.Vendor),
			"Type it yourself once the turn ends.")
		s.openPicker()
		return
	}
	if spelled {
		s.promptBlocked()
	}
	s.log(diag.Rec{Event: diag.EvCommandCaught, Cause: a.rec, Attrs: map[string]any{"command": c.Name, "via": "hook", "asked": true, "spelled": spelled}})
	if c.Kind == sessioncmd.Switch && !s.turnRan {
		// As hookCommand: before a turn end only quitting the CLI completes a
		// switch, and a hook's request must not set that up.
		c = sessioncmd.Command{Kind: sessioncmd.Picker, Name: c.Name}
	}
	// The one step after a hook is the strict case's alone: the keys spelled
	// the line at the prompt, and a live check shows the blocked prompt left
	// the composer empty at this version (#146 review, fourth round).
	s.byteClear = spelled && adapter.BlockedPromptEmpty(s.o.Vendor, s.o.VendorVersion)
	s.runCommand(c, spelled)
	s.byteClear = false
	if c.Kind == sessioncmd.Switch && !spelled && s.sw.to == c.Vendor && !s.sw.typed {
		s.sw.hooked, s.sw.keyed = true, false
	}
}

// dropHookAsk closes the question with no yes (how: esc, typed, or closed when
// something else took aggy's screen) and, unless something else closed it,
// counts it for hookAskMax: a turn end's note or a limit's taking the screen is
// not the person turning it down. The caller closes the picker, unless it is
// closing it.
func (s *session) dropHookAsk(how string) {
	a := s.hookAsk
	if a == nil {
		return
	}
	s.hookAsk = nil
	if how != "closed" {
		s.askDrops = append(s.askDrops, time.Now())
	}
	s.log(diag.Rec{Event: diag.EvCommandAnswered, Cause: a.rec, Attrs: map[string]any{"command": a.c.Name, "answer": how}})
	// Either way the prompt was blocked (#146 review): one the keys spelled
	// leaves the CLI at its prompt, empty, as a yes does; another changes
	// nothing. The key that dropped it comes after.
	if a.spelled && s.enterAt.Equal(a.enterAt) && s.afterEnter == 0 {
		s.promptBlocked()
	}
	if how != "closed" {
		s.closePicker()
	}
}

// aggyItems are /aggy's list: every command, in the CLI's own form, switch,
// borrow, and inbox first. Each runs as /aggy <name> (T6978), which works
// when the person's [commands] leaves the stand-alone name to the CLI; the
// label shows the stand-alone form for those three.
func (s *session) aggyItems() []menuItem {
	pre := sessioncmd.Prefix(s.o.Vendor)
	var items []menuItem
	for _, sub := range sessioncmd.Subcommands {
		label := "aggy " + sub.Name
		if sub.Name == "switch" || sub.Name == "borrow" || sub.Name == "inbox" {
			label = pre + sub.Name
		}
		line := pre + "aggy " + sub.Name
		if sub.Fill {
			line += " "
		}
		items = append(items, menuItem{label: fmt.Sprintf("%-14s %s", label, sub.What), line: line, fill: sub.Fill, key: sub.Name})
	}
	return append(items, menuItem{label: fmt.Sprintf("%-14s %s", pre+"aggy ...", "ask aggy's helper a question (a model turn)"), line: pre + "aggy ", fill: true})
}

// under is the form a command was typed in: "aggy " for /aggy switch and
// the like (T6978), so a line built from it works as typed did.
func under(c sessioncmd.Command) string {
	if strings.HasPrefix(c.Name, "aggy") {
		return "aggy "
	}
	return ""
}

// borrowMenu lists the borrow kinds, filtered to prefix: picking one types
// /borrow <kind> into the composer, in the form c was typed in.
func (s *session) borrowMenu(c sessioncmd.Command, prefix string) *menu {
	pre := sessioncmd.Prefix(s.o.Vendor) + under(c)
	var items []menuItem
	for _, k := range adapter.Kinds() {
		items = append(items, menuItem{label: string(k), line: pre + "borrow " + string(k) + " ", fill: true, key: string(k)})
	}
	return &menu{title: "borrow which kind? Picking one starts the line; write the task and press Enter", items: items, filterable: true, filter: prefix}
}

// openPick opens the list c names, filtered to what was typed (T6978):
// /aggy's commands, the CLIs to switch to, or the borrow kinds. Typing in
// it narrows it further.
func (s *session) openPick(c sessioncmd.Command) {
	switch c.List {
	case "aggy":
		s.showMenu(&menu{title: "commands", items: s.aggyItems(), filterable: true, filter: c.Prefix})
	case "borrow":
		s.showMenu(s.borrowMenu(c, c.Prefix))
	case "switch":
		pre := sessioncmd.Prefix(s.o.Vendor) + under(c)
		var items []menuItem
		for _, v := range s.o.Vendors {
			items = append(items, menuItem{label: string(v), line: pre + "switch " + string(v), key: string(v)})
		}
		s.showMenu(&menu{title: "switch to which CLI?", items: items, filterable: true, filter: c.Prefix})
	}
}

// toolByPrefix takes /aggy tool on|off <name> typed (not picked from aggy's
// list), whose name must be a tool's whole name to run (T6978): it reads the
// tools off the main loop, runs the command when the name is one, and
// otherwise opens the tools list filtered to it. It reports whether it took c.
func (s *session) toolByPrefix(c sessioncmd.Command) bool {
	if c.Sub != "tool" || len(c.Args) != 3 || c.FromList || (c.Args[1] != "on" && c.Args[1] != "off") {
		return false
	}
	if s.open {
		s.closePicker()
	}
	s.cmdSeq++
	pg := &page{title: c.Name, seq: s.cmdSeq, lines: []string{"Reading the tools..."}, busy: true}
	names, seq, pre := s.o.ToolNames, s.cmdSeq, sessioncmd.Prefix(s.o.Vendor)
	s.goSafe(func() {
		r := cmdResult{seq: seq, tools: true, filter: c.Args[2]}
		var all []string
		if names != nil {
			all = names()
		}
		if slices.Contains(all, c.Args[2]) {
			r.tools, r.runCmd = false, &c
		}
		for _, n := range all {
			r.items = append(r.items, menuItem{label: fmt.Sprintf("%-16s turn it %s", n, c.Args[1]), line: pre + "aggy tool " + c.Args[1] + " " + n, key: n})
		}
		select {
		case s.cmdResults <- r:
		case <-s.quit:
		}
	})
	s.pk.list()
	s.pk.page = pg
	s.viewPaste = pasteFilter{}
	s.openPicker()
	return true
}

// cmdArgs are a Run command's arguments, with the session's own ids where
// the command takes them; nil when one is not known.
func (s *session) cmdArgs(c sessioncmd.Command) []string {
	switch c.Sub {
	case "brief":
		id := s.followed
		if id == "" && s.o.SessionID != nil {
			id = s.o.SessionID()
		}
		if id == "" {
			return nil
		}
		return []string{"brief", "--show", "--from", string(s.o.Vendor), "--session", id}
	case "log":
		if s.o.ParentID != "" {
			return []string{"log", "-n", "200", "--parent", s.o.ParentID}
		}
		return []string{"log", "-n", "200"}
	}
	return c.Args
}

// showMenu puts a menu on screen.
func (s *session) showMenu(m *menu) {
	if s.open {
		s.closePicker()
	}
	s.pk.list()
	s.pk.menu = m
	s.viewPaste = pasteFilter{}
	s.openPicker()
}

// startRun puts a page on screen and runs args off the main loop; their output
// replaces the page's text. A ToolList command reads the tools instead and
// becomes their menu. A command with Text shows it and runs nothing until y.
func (s *session) startRun(c sessioncmd.Command, args []string) {
	if s.open {
		s.closePicker()
	}
	s.cmdSeq++
	pg := &page{title: c.Name, seq: s.cmdSeq, confirm: c.Confirm, confirmLabel: c.ConfirmLabel, confirmIf: c.ConfirmIf, answer: c.Answer, also: c.Also, alsoLabel: c.AlsoLabel}
	switch {
	case c.Text != "":
		pg.lines = []string{c.Text}
	case c.Kind == sessioncmd.ToolList:
		pg.lines, pg.busy = []string{"Reading the tools..."}, true
		s.goTools(pg.seq)
	case len(args) == 0:
		pg.lines = []string{"aggy does not know this session's id yet, so it cannot show that."}
	default:
		pg.lines, pg.busy = []string{"Running aggy " + strings.Join(args, " ") + "..."}, true
		s.goRun(pg.seq, args, false)
	}
	s.pk.list()
	s.pk.page = pg
	s.viewPaste = pasteFilter{}
	s.openPicker()
}

// cmdResult is what a run printed, or the tools read, for run seq.
type cmdResult struct {
	seq   int
	out   string
	tools bool
	items []menuItem
	// trust is the trust list read for /aggy trust.
	trust *TrustList
	// filter narrows the tools listed (toolByPrefix); runCmd is the command
	// to run once the tool's name was found whole.
	filter string
	runCmd *sessioncmd.Command
}

func (s *session) goRun(seq int, args []string, answer bool) {
	// TODO(T6954): a panic in the in-process command ends the whole session
	// (goSafe re-panics), and a slow one (a check) holds its page until done.
	run := s.o.Aggy
	s.goSafe(func() {
		out := run(args, answer)
		select {
		case s.cmdResults <- cmdResult{seq: seq, out: out}:
		case <-s.quit:
		}
	})
}

func (s *session) goTools(seq int) {
	names, now, pre := s.o.ToolNames, s.o.Tools, sessioncmd.Prefix(s.o.Vendor)
	s.goSafe(func() {
		var items []menuItem
		var on ToolSet
		if now != nil {
			on, _ = now()
		}
		if names != nil {
			for _, n := range names() {
				op, what := "on", "off here; picking turns it on"
				if on[n] != "" {
					op, what = "off", "on here; picking turns it off"
				}
				items = append(items, menuItem{label: fmt.Sprintf("%-16s %s", n, what), line: pre + "aggy tool " + op + " " + n})
			}
		}
		select {
		case s.cmdResults <- cmdResult{seq: seq, tools: true, items: items}:
		case <-s.quit:
		}
	})
}

// cmdDone takes a run's result: it goes on the page it was for, if that page
// is still up.
func (s *session) cmdDone(r cmdResult) {
	g := s.pk.page
	if !s.open || g == nil || g.seq != r.seq {
		return
	}
	g.busy = false
	switch {
	case r.runCmd != nil:
		s.startRun(*r.runCmd, s.cmdArgs(*r.runCmd))
		return
	case r.tools && len(r.items) == 0:
		g.lines = []string{"No managed tools are defined. aggy tool list shows them."}
	case r.tools:
		s.pk.page = nil
		s.pk.menu = &menu{title: "turn a tool on or off (a resume of this CLI picks it up)", items: r.items, filterable: true, filter: r.filter}
	case r.trust != nil:
		s.pk.page = nil
		s.pk.menu = &menu{title: g.title, check: newCheckList(r.trust.Lines, r.trust.State)}
	default:
		if s.fixDone(g, r.out) {
			return
		}
		g.lines, g.top = plainText(r.out), 0
		if g.confirmIf != "" && !strings.Contains(r.out, g.confirmIf) {
			g.confirm, g.also = nil, nil // it asked nothing, so y has nothing to answer
		}
		g.confirmIf = ""
	}
	s.redrawView()
}

func (s *session) redrawView() {
	rows, cols := s.t.size()
	s.pk.drawView(s.o.Out, s.pickerOverCLI, rows, cols)
}

// closeView takes the menu or page off the screen, back to the CLI.
func (s *session) closeView() {
	s.pk.menu, s.pk.page = nil, nil
	s.closePicker()
}

// viewInput takes keys at a menu or a page. A paste is dropped: it never picks.
func (s *session) viewInput(b []byte) {
	keys, runes := viewKeys(s.viewPaste.drop(b), s.pk.key)
	if s.pk.menu != nil && s.pk.menu.check != nil && !checkBurst(keys, runes) {
		// A burst, or a key aggy does not know: typing, as at a first start;
		// but Esc or Ctrl-C in it still closes the list.
		k := keyNone
		if slices.Contains(keys, keyClose) {
			k = keyClose
		}
		keys, runes = []viewKey{k}, []rune{0}
	}
	for i, k := range keys {
		var done bool
		switch {
		case s.pk.menu != nil:
			done = s.menuKey(k, runes[i])
		case s.pk.page != nil:
			done = s.pageKey(k, runes[i])
		default:
			return
		}
		if done {
			return
		}
	}
	if s.open && (s.pk.menu != nil || s.pk.page != nil) {
		s.redrawView()
	}
}

// menuKey takes one key at a menu, and reports whether the menu is gone.
func (s *session) menuKey(k viewKey, r rune) bool {
	m := s.pk.menu
	if m.check != nil {
		return s.checkKey(k, r)
	}
	items := m.shown()
	switch {
	case k == keyClose || r == 'q' && !m.filterable:
		s.closeView()
		return true
	case k == keyUp || r == 'k' && !m.filterable:
		m.at = max(m.at-1, 0)
	case k == keyDown || r == 'j' && !m.filterable:
		m.at = max(min(m.at+1, len(items)-1), 0)
	case k == keyEnter && len(items) > 0:
		s.choose(items[min(m.at, len(items)-1)])
		return true
	case r >= '1' && r <= '9' && int(r-'1') < len(items):
		s.choose(items[r-'1'])
		return true
	case k == keyBackspace && m.filterable && m.filter != "":
		// Typing narrows a list opened from what was typed (T6978).
		m.filter, m.at = m.filter[:len(m.filter)-1], 0
	case m.filterable && (r >= 'a' && r <= 'z' || r == '-' || r == '_'):
		m.filter, m.at = m.filter+string(r), 0
	}
	return false
}

// pageKey takes one key at a page, and reports whether the page is gone.
func (s *session) pageKey(k viewKey, r rune) bool {
	g := s.pk.page
	if !g.opened.IsZero() && time.Since(g.opened) < checkTypeAhead {
		return false // typed before the fix page was there
	}
	rows, _ := s.t.size()
	if rows < 3 {
		rows = screenRows
	}
	room := max(rows-4, 1)
	switch {
	case k == keyClose || r == 'q':
		s.closeView()
		return true
	case k == keyUp || r == 'k':
		g.top = max(g.top-1, 0)
	case k == keyDown || r == 'j':
		g.top++
	case k == keyPageDown || r == ' ':
		g.top += room
	case k == keyPageUp || r == 'b':
		g.top = max(g.top-room, 0)
	case r == 'g':
		g.top = 0
	case r == 'G':
		g.top = len(g.lines)
	case r == 'c' && len(g.also) > 0 && !g.busy:
		// The step before y (accept's checks): y stays for once it is done.
		args := g.also
		s.cmdSeq++
		g.seq, g.busy, g.also, g.top = s.cmdSeq, true, nil, 0
		g.lines = []string{"Running aggy " + strings.Join(args, " ") + "..."}
		s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": g.title, "via": "keys", "step": args[0]}})
		s.goRun(g.seq, args, false)
	case r == 'y' && len(g.confirm) > 0 && !g.busy:
		args := g.confirm
		s.cmdSeq++
		g.seq, g.busy, g.confirm, g.also, g.top = s.cmdSeq, true, nil, nil, 0
		g.lines = []string{"Running aggy " + strings.Join(args, " ") + "..."}
		s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": g.title, "via": "keys", "confirmed": true}})
		s.goRun(g.seq, args, g.answer)
	}
	return false
}

// choose acts on a menu's item: a command runs, the start of one is typed
// into the composer for the person to finish.
func (s *session) choose(it menuItem) {
	s.closeView()
	if it.fill {
		s.fillComposer(it.line)
		return
	}
	c, ok := sessioncmd.Parse(s.o.Vendor, it.line)
	if !ok || !s.takes(c) {
		return
	}
	c.FromList = true
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": c.Name, "via": "list"}})
	s.runCommand(c, true)
}

// fillComposer types line into the composer, as the person's own keys, when
// aggy knows it is empty; otherwise it says what to type.
func (s *session) fillComposer(line string) {
	empty := s.lenKnown && s.compLen == 0 && s.draftLen == 0 && !s.draftBlind && !s.doubt() && !s.askOpen() && !s.exitGap
	if !empty {
		s.cmdSeq++
		s.pk.list()
		s.pk.page = &page{title: "aggy", seq: s.cmdSeq, lines: []string{
			"aggy cannot tell that the prompt is empty, so it typed nothing.",
			"Clear the prompt and type: " + strings.TrimSpace(line)}}
		s.openPicker()
		return
	}
	s.send([]byte(line))
}

// menuPick is what the person typed at the prompt for a pick from the CLI's own
// menu (T6975, docs/design.md section 5), which the keys do not spell: text, the
// printable keys typed since the line began, before any menu key; menu, that
// Up or Down or a Tab came after them; tab, that a Tab did, and args, the
// printable keys typed after it. seq holds an escape sequence still coming in.
//
// bad: something else came, so the line is no pick aggy can tell from its
// keys: a paste, Esc, a cursor key other than Up and Down (Left moves into the
// text), a menu key before any text (Up there recalls history) or after a space
// (the command word has ended, and the menu with it), text typed after Up or
// Down but before a Tab (the menu filters again, or a recalled line grows), a
// key after a Tab other than text and Backspace, an edit other than a
// Backspace at the end, Ctrl+U, a control key, a click, or a newline. why
// names the first of these, as a fixed word for a record (T6982).
type menuPick struct {
	text, args []byte
	menu, tab  bool
	bad        bool
	why        string
	seq        []byte
}

// spoil marks the pick bad, keeping the first reason.
func (p *menuPick) spoil(why string) {
	if !p.bad {
		p.why = why
	}
	p.bad = true
}

// menuArrow matches Up or Down as a legacy terminal or kitty sends it, a press
// or a repeat with no modifier but Caps or Num Lock; menuArrowRelease is its
// release, which kitty's report-all mode sends and which changes nothing.
var (
	menuArrow        = regexp.MustCompile(`^\x1b(\[(1;(1|65|129|193)(:[12])?)?|O)[AB]$`)
	menuArrowRelease = regexp.MustCompile(`^\x1b\[1;(1|65|129|193):3[AB]$`)
)

// pickByte follows one byte the person sent, for menuPick.
func (s *session) pickByte(c byte) {
	p := &s.pick
	if s.inPaste {
		p.spoil("paste")
		return
	}
	if len(p.seq) > 0 || c == esc {
		p.seq = append(p.seq, c)
		if !seqDone(p.seq) {
			return
		}
		seq := string(p.seq)
		p.seq = nil
		switch {
		case menuArrowRelease.MatchString(seq):
		case menuArrow.MatchString(seq):
			p.menuKey(false)
		default:
			p.spoil("other_key")
		}
		return
	}
	switch {
	case p.bad, c == '\r':
	case c == '\t':
		p.menuKey(true)
	case c == 0x7f || c == 0x08:
		switch {
		case p.tab && len(p.args) > 0:
			p.args = p.args[:len(p.args)-1]
		case !p.menu && len(p.text) > 0:
			p.text = p.text[:len(p.text)-1]
		default:
			p.spoil("backspace")
		}
	case len(p.text)+len(p.args) >= sessioncmd.MaxLine:
		p.spoil("too_long")
	case c < 0x20 || c > 0x7e:
		p.spoil("control_key")
	case p.tab:
		p.args = append(p.args, c)
	case p.menu:
		p.spoil("text_after_menu")
	default:
		p.text = append(p.text, c)
	}
}

// menuKey takes Up, Down (tab false), or a Tab: allowed after the start of a
// command word, before a space and before any Tab.
func (p *menuPick) menuKey(tab bool) {
	switch {
	case len(p.text) == 0:
		p.spoil("menu_key_first")
	case p.tab, bytes.IndexByte(p.text, ' ') >= 0:
		p.spoil("menu_key_late")
	default:
		p.menu, p.tab = true, tab
	}
}

// seqDone reports whether seq, which starts with ESC, is a whole escape
// sequence: CSI to its final byte, SS3 and any other two bytes whole. A long
// one is done too, and none that menuPick takes.
func seqDone(seq []byte) bool {
	switch {
	case len(seq) < 2:
		return false
	case len(seq) > 32:
		return true
	case seq[1] == '[':
		return len(seq) > 2 && seq[len(seq)-1] >= 0x40 && seq[len(seq)-1] <= 0x7e
	case seq[1] == 'O':
		return len(seq) == 3
	}
	return true
}

// promptFree reports whether line, a command adapter.LocalCommand lists for v,
// leaves the keys with the prompt at once, so a command typed after it is the
// composer's (lineKnown, cmdFresh). Only Claude's /clear: commands.md ("All
// commands", read 2026-11-07) says it starts "a new conversation with empty
// context", and describes no dialog. /model "opens a picker" with no
// argument, /config and /status open "the Settings interface", /permissions
// "an interactive dialog", and /memory edits CLAUDE.md files; /usage, /cost,
// /help, and /context show a panel or a display whose focus after it the docs
// do not say, so they take the safe side: a command typed next goes to the CLI
// until a turn end (T6975).
func promptFree(v adapter.Vendor, line string) bool {
	return v == adapter.Claude && line == "/clear"
}
