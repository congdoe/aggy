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
	s.runCommand(c, true)
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
// lets the prompt through rather than the hook timing out.
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
// from the vendor on screen, only where aggy installs that hook (Claude and
// Grok), and never a keyboard-only one, since any process in the CLI's tree
// can send it. The main loop then decides (hookCommand), and a nil answer
// makes aggy hook block the prompt; any other lets it through.
func (s *session) onPromptCommand(r Request) error {
	on := s.o.Vendor
	if r.Vendor != string(on) {
		return coded(diag.CodeWrongVendor, fmt.Errorf("a command from %s's hook, but %s is on screen", r.Vendor, on))
	}
	if on != adapter.Claude && on != adapter.Grok {
		return coded(diag.CodeCommandNotTaken, fmt.Errorf("aggy installs no prompt hook for %s", on))
	}
	c, ok := sessioncmd.Parse(on, r.Command)
	switch {
	case ok && c.KeyboardOnly:
		return coded(diag.CodeCommandKeyboard, fmt.Errorf("%s changes something, so aggy takes it only typed in full at the prompt", c.Name))
	case !ok || !s.takes(c):
		return coded(diag.CodeCommandNotTaken, errors.New("not a command aggy takes from a hook"))
	}
	p := promptReq{c: c, line: r.Command, answer: make(chan error, 1), claimed: new(atomic.Bool)}
	select {
	case s.prompts <- p:
	default:
		return coded(diag.CodeCommandNotTaken, errors.New("another command from a hook is still waiting"))
	}
	t := time.NewTimer(promptAnswerWait)
	defer t.Stop()
	select {
	case err := <-p.answer:
		return err
	case <-t.C:
	}
	if p.claimed.CompareAndSwap(false, true) {
		return coded(diag.CodeCommandNotTaken, errors.New("aggy was busy, so the prompt went to the CLI"))
	}
	return <-p.answer // the main loop claimed it first and answers now
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
// then perhaps Up, Down, or Tab, and after a Tab its arguments. So a forged
// request after the person's own /deploy or /clear names nothing they typed,
// and is refused. Nothing is taken over a list, page, or picker already on
// screen, nor a switch over one waiting. The hook then blocked that prompt, so
// that Enter started no turn: the prompt is where it was, and its line is gone
// from the composer (both CLIs document a block as stopping the prompt; that
// the composer is left empty is unverified, TODO(T6895)). A switch caught this
// way always asks on the status line.
func (s *session) hookCommand(c sessioncmd.Command, line string) error {
	// Claude may report one prompt twice, as submitted and as a command's
	// expansion (UserPromptSubmit and UserPromptExpansion, hooks.md; which of
	// them a menu pick sends is unverified). The second blocks too and does
	// nothing more.
	if c.Name == s.hookedName && s.hookedEnter.Equal(s.enterAt) && time.Since(s.enterAt) < promptRepeat {
		return nil
	}
	switch {
	case !s.enterAtPrompt || s.enterPick.bad || s.afterEnter != 0 || time.Since(s.enterAt) >= promptBlockWait:
		s.hookMiss(c, []string{"hook_no_enter"})
		return coded(diag.CodeCommandNoEnter, errors.New("no Enter of a / command at the prompt just before it"))
	case !sessioncmd.Picked(s.o.Vendor, string(s.enterPick.text), s.enterPick.tab, string(s.enterPick.args), line):
		s.hookMiss(c, []string{"hook_keys_differ"})
		return coded(diag.CodeCommandNoEnter, errors.New("the keys typed before that Enter do not start that command"))
	case s.hookedEnter.Equal(s.enterAt):
		return coded(diag.CodeCommandNoEnter, errors.New("aggy already took a command for that Enter"))
	case s.open:
		return coded(diag.CodeCommandNotTaken, errors.New("aggy's list or page is open"))
	case (c.Kind == sessioncmd.Switch || c.Kind == sessioncmd.Picker) && s.sw.to != "":
		return coded(diag.CodeCommandNotTaken, errors.New("a switch is already waiting"))
	}
	s.hookedName, s.hookedEnter = c.Name, s.enterAt
	s.atPrompt = !s.held
	// TODO(T6975): a blocked prompt is taken as an empty composer, unverified;
	// if the CLI puts the text back, an exit command is typed after it.
	s.emptyComposer()
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
		s.pk.menu = &menu{title: g.title, check: &checkList{lines: r.trust.Lines, st: r.trust.State, opened: time.Now()}}
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
// Backspace at the end, Ctrl+U, a control key, a click, or a newline.
type menuPick struct {
	text, args []byte
	menu, tab  bool
	bad        bool
	seq        []byte
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
		p.bad = true
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
			p.bad = true
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
			p.bad = true
		}
	case c < 0x20 || c > 0x7e, len(p.text)+len(p.args) >= sessioncmd.MaxLine:
		p.bad = true
	case p.tab:
		p.args = append(p.args, c)
	case p.menu:
		p.bad = true
	default:
		p.text = append(p.text, c)
	}
}

// menuKey takes Up, Down (tab false), or a Tab: allowed after the start of a
// command word, before a space and before any Tab.
func (p *menuPick) menuKey(tab bool) {
	if len(p.text) == 0 || p.tab || bytes.IndexByte(p.text, ' ') >= 0 {
		p.bad = true
		return
	}
	p.menu, p.tab = true, tab
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
