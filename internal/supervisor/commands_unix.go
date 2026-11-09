//go:build unix

package supervisor

import (
	"errors"
	"fmt"
	"strings"
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
	if s.o.Vendor == adapter.Claude || s.o.Vendor == adapter.Grok {
		// Claude and Grok render inline, so on the alternate screen the keys go
		// to an editor or a pager the CLI opened, where / searches.
		s.outMu.Lock()
		alt := s.alt.on
		s.outMu.Unlock()
		if alt {
			return false
		}
	}
	clear := []byte{ctrlU}
	if raw[0] == esc {
		// The terminal reports keys as CSI u: Ctrl+U the same way (unverified
		// that each CLI takes it so; its legacy byte may also do).
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
func (s *session) lineKnown() bool {
	switch {
	case len(s.cmdLine) == 0, s.cmdBad, !s.lenKnown, s.draftBlind, s.draftPaste, s.inPaste, s.pastePop:
		return false
	case s.cleared, s.escDoubt, s.escEmpty, s.escTwice, s.moved, s.askOpen(), s.exitGap:
		return false
	}
	return s.compLen == len(s.cmdLine) && s.draftLen == len(s.cmdLine)
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
	s.toldDirty = false
	s.updateDirty()
}

// takes reports whether this session acts on c. Each kind is taken only where
// it has something to act with.
func (s *session) takes(c sessioncmd.Command) bool {
	switch c.Kind {
	case sessioncmd.Picker, sessioncmd.Switch:
		return true
	case sessioncmd.Run, sessioncmd.AggyList, sessioncmd.BorrowList, sessioncmd.ToolList:
		return s.o.Aggy != nil
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
		s.pickSwitch(Choice{Vendor: c.Vendor, Brief: c.Brief, New: c.New, Model: c.Model}, keyed)
	case sessioncmd.AggyList:
		s.showMenu(&menu{title: "commands", items: s.aggyItems()})
	case sessioncmd.BorrowList:
		var items []menuItem
		for _, k := range adapter.Kinds() {
			items = append(items, menuItem{label: string(k), line: sessioncmd.Prefix(s.o.Vendor) + "borrow " + string(k) + " ", fill: true})
		}
		s.showMenu(&menu{title: "borrow which kind? Picking one starts the line; write the task and press Enter", items: items})
	case sessioncmd.ToolList:
		s.startRun(c, nil)
	case sessioncmd.Run:
		s.startRun(c, s.cmdArgs(c))
	}
}

// promptBlockWait bounds how long after the Enter a prompt hook's command
// may say that Enter started no turn. The hook runs before the model
// (Claude hooks.md, UserPromptSubmit), within aggy hook's few seconds.
const promptBlockWait = 10 * time.Second

// promptRepeat is how soon the same command from a prompt hook counts as the
// same prompt reported again.
const promptRepeat = 2 * time.Second

// onPromptCommand takes a command a CLI's UserPromptSubmit hook reported
// (T6910), on a connection's goroutine: the line is parsed again here, only
// from the vendor on screen, only where aggy installs that hook (Claude and
// Grok), and never a keyboard-only one, since any process in the CLI's tree
// can send it. A nil answer makes aggy hook block the prompt.
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
	case !ok || !s.takes(c):
		return coded(diag.CodeCommandNotTaken, errors.New("not a command aggy takes from a hook"))
	case c.KeyboardOnly:
		return coded(diag.CodeCommandKeyboard, fmt.Errorf("%s changes something, so aggy takes it only typed in full at the prompt", c.Name))
	}
	select {
	case s.prompts <- c:
		return nil
	default:
		return coded(diag.CodeCommandNotTaken, errors.New("another command from a hook is still waiting"))
	}
}

// hookCommand acts on a command a prompt hook reported. The hook blocked the
// prompt, so the Enter that sent it started no turn: when it went at the
// prompt, just now, with nothing typed since, the prompt is where it was. A
// switch caught this way always asks on the status line: a hook can be forged.
func (s *session) hookCommand(c sessioncmd.Command) {
	// Claude may report one prompt twice, as submitted and as a command's
	// expansion (UserPromptSubmit and UserPromptExpansion, hooks.md; which of
	// them a menu pick sends is unverified). The second does nothing.
	if c.Name == s.hookedName && time.Since(s.hookedAt) < promptRepeat {
		return
	}
	s.hookedName, s.hookedAt = c.Name, time.Now()
	if s.enterAtPrompt && s.afterEnter == 0 && time.Since(s.enterAt) < promptBlockWait {
		s.atPrompt = !s.held
	}
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": c.Name, "via": "hook"}})
	if c.Kind == sessioncmd.Switch && !s.turnRan {
		// Before a turn end only quitting the CLI completes a switch, and a
		// forged hook must not set that up: the list says it, and a pick there
		// is the person's.
		c = sessioncmd.Command{Kind: sessioncmd.Picker, Name: c.Name}
	}
	s.runCommand(c, false)
	if c.Kind == sessioncmd.Switch && s.sw.to == c.Vendor && !s.sw.typed {
		// The line went through the CLI's own menu, whose Enter leaves the
		// composer in doubt; whether a blocked prompt's text comes back to it
		// is unverified. The status line asks anyway, so it says that too,
		// rather than a note first that only asks for a second pick.
		s.sw.hooked, s.sw.keyed, s.sw.doubtOK = true, false, true
	}
}

// aggyItems are /aggy's list: every command, in the CLI's own form.
func (s *session) aggyItems() []menuItem {
	pre := sessioncmd.Prefix(s.o.Vendor)
	items := []menuItem{
		{label: fmt.Sprintf("%-14s %s", pre+"switch", "switch to another CLI"), line: pre + "switch"},
		{label: fmt.Sprintf("%-14s %s", pre+"borrow", "borrow a task to another CLI (a model turn)"), line: pre + "borrow"},
		{label: fmt.Sprintf("%-14s %s", pre+"inbox", "borrow reports you have not read"), line: pre + "inbox"},
	}
	for _, sub := range sessioncmd.Subcommands {
		line := pre + "aggy " + sub.Name
		fill := sub.Name == "check" || sub.Name == "accept" || sub.Name == "reject"
		if fill {
			line += " "
		}
		items = append(items, menuItem{label: fmt.Sprintf("%-14s %s", "aggy "+sub.Name, sub.What), line: line, fill: fill})
	}
	return append(items, menuItem{label: fmt.Sprintf("%-14s %s", pre+"aggy ...", "ask aggy's helper a question (a model turn)"), line: pre + "aggy ", fill: true})
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
	pg := &page{title: c.Name, seq: s.cmdSeq, confirm: c.Confirm, confirmLabel: c.ConfirmLabel, answer: c.Answer}
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
}

func (s *session) goRun(seq int, args []string, answer bool) {
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
	case r.tools && len(r.items) == 0:
		g.lines = []string{"No managed tools are defined. aggy tool list shows them."}
	case r.tools:
		s.pk.page = nil
		s.pk.menu = &menu{title: "turn a tool on or off (a resume of this CLI picks it up)", items: r.items}
	default:
		g.lines, g.top = plainText(r.out), 0
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
	switch {
	case k == keyClose || r == 'q':
		s.closeView()
		return true
	case k == keyUp || r == 'k':
		m.at = max(m.at-1, 0)
	case k == keyDown || r == 'j':
		m.at = max(min(m.at+1, len(m.items)-1), 0)
	case k == keyEnter && len(m.items) > 0:
		s.choose(m.items[m.at])
		return true
	case r >= '1' && r <= '9' && int(r-'1') < len(m.items):
		s.choose(m.items[r-'1'])
		return true
	}
	return false
}

// pageKey takes one key at a page, and reports whether the page is gone.
func (s *session) pageKey(k viewKey, r rune) bool {
	g := s.pk.page
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
	case r == 'y' && len(g.confirm) > 0 && !g.busy:
		args := g.confirm
		s.cmdSeq++
		g.seq, g.busy, g.confirm, g.top = s.cmdSeq, true, nil, 0
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
