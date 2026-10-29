//go:build unix

package supervisor

import (
	"fmt"
	"io"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// When the CLI is on the main screen, the picker is drawn on the
// alternate screen, so leaving it puts back the CLI's screen and cursor
// exactly, without anyone redrawing. The alternate screen does not nest,
// so when the CLI is already on it (Codex without --no-alt-screen, or an
// editor or pager a CLI opened), the picker is drawn over the CLI's
// screen instead, and the CLI is made to repaint itself afterwards (see
// session.closePicker), which puts back its cursor too. The cursor is not
// saved with DECSC: that slot is the CLI's own, and a full-screen CLI or
// pager may hold a position there (B13-13).
const (
	altScreenOn  = "\x1b[?1049h\x1b[H\x1b[2J"
	altScreenOff = "\x1b[?1049l"
)

// picker is the minimal switch menu: the connected vendors in switch.order
// order, a number or Enter picks one, and Esc, Ctrl+C, or the switch key
// cancels. The same overlay asks to confirm a switch at a turn end
// (confirm set) and shows a note (note set) that any key closes.
type picker struct {
	vendors []adapter.Vendor
	key     byte
	// children lists a vendor's children in the open parent (T6665).
	children func(adapter.Vendor) []ChildChoice
	// opts are the list's toggles for the next pick: n (new child), b
	// (brief), m (model, typed in edit mode).
	opts    Choice
	editing bool
	edit    []byte
	editErr string
	// stage, when set, is the vendor whose children the list shows.
	stage adapter.Vendor
	kids  []ChildChoice
	// preselect is the vendor Enter picks (a limit's suggestion, T6667);
	// empty means the first.
	preselect adapter.Vendor
	// waiting is a picked switch waiting for the turn end, shown in the
	// list; Esc there cancels it.
	waiting adapter.Vendor
	// checking: a check of waiting (the policy probe) is still running.
	checking bool
	// confirm is the vendor a confirmation is for; lines explain it.
	confirm adapter.Vendor
	note    bool
	lines   []string
	// fit, when set, is the confirmation's status line for a width, cut
	// where it should be (T6770); statusLine still cuts its end.
	fit func(cols int) string
	// warning: the note is a near-limit warning (T6699), drawn on the
	// status line as the first of forms that fits the width; the switch
	// key opens the list with warnTo, if any, preselected.
	warning bool
	warnTo  adapter.Vendor
	forms   []string
}

// list sets the picker to the vendor list, its toggles cleared.
func (p *picker) list() {
	p.confirm, p.note, p.lines = "", false, nil
	p.warning, p.warnTo, p.forms, p.fit = false, "", nil, nil
	p.opts, p.editing, p.edit, p.editErr, p.stage, p.kids = Choice{}, false, nil, "", "", nil
}

// ask sets the picker to confirm a switch to v, explained by lines.
func (p *picker) ask(v adapter.Vendor, lines ...string) {
	p.confirm, p.note, p.lines = v, false, lines
	p.warning, p.warnTo, p.forms, p.fit = false, "", nil, nil
}

// tell sets the picker to show a note.
func (p *picker) tell(lines ...string) {
	p.confirm, p.note, p.lines = "", true, lines
	p.warning, p.warnTo, p.forms, p.fit = false, "", nil, nil
}

// warn sets the picker to a near-limit warning (T6699): a note on the
// status line whose switch key preselects to. forms are the line from
// longest to shortest, each ASCII and ending in its keys.
func (p *picker) warn(to adapter.Vendor, forms ...string) {
	p.tell(forms[0])
	p.warning, p.warnTo, p.forms = true, to, forms
}

// draw writes the picker: on the alternate screen, or, when the CLI is
// already on it (overCLI), over the top lines of the CLI's screen.
func (p *picker) draw(w io.Writer, overCLI bool) {
	var lines []string
	switch {
	case p.confirm != "":
		lines = append(append([]string{"aggy: switch to " + string(p.confirm), ""}, p.lines...),
			"", fmt.Sprintf("Press %s to switch, Esc to stay. Anything else you type goes to the CLI.", keyName(p.key)))
	case p.note:
		lines = append(append([]string{"aggy", ""}, p.lines...), "", "Esc closes this. Anything else you type goes to the CLI.")
	case p.stage != "":
		lines = []string{fmt.Sprintf("aggy: switch to %s: which session?", p.stage), ""}
		for i, k := range p.kids {
			if i == 9 {
				break
			}
			lines = append(lines, fmt.Sprintf("  %d  %s", i+1, k.Label))
		}
		lines = append(lines, fmt.Sprintf("  n  a new %s session", p.stage), "",
			"Number to pick, Enter for the latest, Esc to go back.")
	default:
		lines = []string{"aggy: switch vendor", ""}
		if len(p.vendors) == 0 {
			lines = append(lines, "  No other connected vendor. `aggy setup` checks them.")
		}
		for i, v := range p.vendors {
			mark := " "
			if v == p.preselect {
				mark = ">"
			}
			lines = append(lines, fmt.Sprintf(" %s%d  %s", mark, i+1, v))
		}
		if p.waiting != "" {
			lines = append(lines, "", fmt.Sprintf("Switching to %s when this turn ends; Esc cancels it, picking it again says no turn is running.", p.waiting))
			if p.checking {
				lines = append(lines, fmt.Sprintf("Checking that %s can get the safety policy...", p.waiting))
			}
		}
		model := "the vendor's default"
		if p.opts.Model != "" {
			model = p.opts.Model
		}
		lines = append(lines, "", fmt.Sprintf("n  new session: %s    b  brief: %s    m  model for this launch: %s",
			onOff(p.opts.New), onOff(p.opts.Brief), model))
		if p.editing {
			lines = append(lines, fmt.Sprintf("Model: %s_   (Enter sets it, Esc cancels)", p.edit))
			if p.editErr != "" {
				lines = append(lines, p.editErr)
			}
		}
		lines = append(lines, "", "Number or Enter to switch at the turn end, Esc to go back.")
	}
	var b strings.Builder
	if overCLI {
		for i, l := range lines {
			fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K%s", i+1, l)
		}
	} else {
		b.WriteString(altScreenOn)
		for _, l := range lines {
			b.WriteString(l + "\r\n")
		}
	}
	_, _ = io.WriteString(w, b.String())
}

// statusLine is the one line of a confirmation (S-12) or a near-limit
// warning: it never hides the CLI's screen, so an approval prompt or menu
// the CLI shows stays in view while the user decides. A warning takes the
// first of its forms that fits, so a narrow terminal gets a shorter line
// that still ends in its keys (L60-4); anything else is cut at the end.
func (p *picker) statusLine(cols int) string {
	line := "aggy: " + strings.Join(p.lines, " ")
	if p.fit != nil {
		line = p.fit(cols)
	}
	for _, f := range p.forms {
		if line = f; cols <= 1 || len(f) <= cols-1 {
			return f
		}
	}
	if cols > 1 && len(line) > cols-1 {
		line = line[:cols-1] // ASCII only; never writes the last column, so no wrap or scroll
	}
	return line
}

// drawStatus writes the confirmation on the bottom row. On the CLI's
// main screen the cursor is saved and put back (DECSC, DECRC), so an
// inline renderer's relative moves still line up; on its alternate
// screen the CLI repaints the whole screen afterwards anyway.
//
// shortcut: DECSC takes the CLI's own save slot for the moment the line
// is drawn; a CLI holding a position there loses it. Upgrade if T6684
// finds one.
func (p *picker) drawStatus(w io.Writer, rows, cols int, onAlt bool) {
	at := fmt.Sprintf("\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m", rows, p.statusLine(cols))
	if !onAlt {
		at = "\x1b7" + at + "\x1b8"
	}
	_, _ = io.WriteString(w, at)
}

// eraseStatus clears the bottom row the confirmation used.
func (p *picker) eraseStatus(w io.Writer, rows int, onAlt bool) {
	at := fmt.Sprintf("\x1b[%d;1H\x1b[2K", rows)
	if !onAlt {
		at = "\x1b7" + at + "\x1b8"
	}
	_, _ = io.WriteString(w, at)
}

// erase takes the picker off the screen: it leaves the alternate screen,
// or, when drawn over the CLI, does nothing (the CLI repaints).
func (p *picker) erase(w io.Writer, overCLI bool) {
	if overCLI {
		return
	}
	_, _ = io.WriteString(w, altScreenOff)
}

// keyName names a switch key, such as Ctrl+\.
func keyName(k byte) string { return "Ctrl+" + string(rune(k+0x40)) }

// isKey reports whether b is one press of the switch key, as its control
// byte or its kitty report.
func (p *picker) isKey(b []byte) bool {
	if len(b) == 1 {
		return b[0] == p.key
	}
	r, ok := parseCSIu(b)
	return ok && r.n == len(b) && r.code == kittyCode(p.key) && r.mods == 5
}

// onOff names a toggle's state.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// handle reads one chunk of input. It returns done when the picker should
// close, and the switch chosen, if any (Vendor empty for none). It reads
// the list only; a confirmation or note is session.promptInput's. Toggles
// and the model edit change the picker, which the caller redraws.
func (p *picker) handle(b []byte) (done bool, c Choice) {
	if p.editing {
		p.editKeys(b)
		return false, Choice{}
	}
	if len(b) == 1 && b[0] == esc {
		if p.stage != "" {
			p.stage, p.kids = "", nil
			return false, Choice{}
		}
		return true, Choice{}
	}
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if ch == esc {
			r, ok := parseCSIu(b[i:])
			if !ok {
				return false, Choice{} // another escape sequence: ignore the rest
			}
			i += r.n - 1
			switch {
			case r.code == 27 || r.code == 'c' && r.mods == 5 || r.code == kittyCode(p.key) && r.mods == 5:
				return true, Choice{}
			case r.code == 13 || r.code == 57414: // Enter, keypad Enter
				ch = '\r'
			case r.code >= '1' && r.code <= '9' || r.code == 'n' || r.code == 'b' || r.code == 'm':
				ch = byte(r.code)
			default:
				continue
			}
		}
		switch {
		case ch == 0x03 || ch == p.key:
			return true, Choice{}
		case p.stage != "":
			n := min(len(p.kids), 9)
			switch {
			case ch == '\r' && n > 0:
				return p.pick(p.stage, p.kids[0].ID, false)
			case ch == 'n':
				return p.pick(p.stage, "", true)
			case ch >= '1' && ch <= '9' && int(ch-'0') <= n:
				return p.pick(p.stage, p.kids[ch-'1'].ID, false)
			}
		case ch == 'n':
			p.opts.New = !p.opts.New
		case ch == 'b':
			p.opts.Brief = !p.opts.Brief
		case ch == 'm':
			p.editing, p.edit, p.editErr = true, []byte(p.opts.Model), ""
			p.editKeys(b[i+1:])
			return false, Choice{}
		case ch == '\r' && len(p.vendors) > 0:
			v := p.vendors[0]
			if p.preselect != "" {
				v = p.preselect
			}
			return p.choose(v)
		case ch >= '1' && ch <= '9' && int(ch-'0') <= len(p.vendors):
			return p.choose(p.vendors[ch-'1'])
		}
	}
	return false, Choice{}
}

// choose picks v with the list's toggles, or, when v has several
// children in the parent and no new one is asked for, shows them.
func (p *picker) choose(v adapter.Vendor) (bool, Choice) {
	if !p.opts.New && p.children != nil {
		if kids := p.children(v); len(kids) > 1 {
			p.stage, p.kids = v, kids
			return false, Choice{}
		}
	}
	return p.pick(v, "", p.opts.New)
}

// pick is the choice of v's child (empty: the latest) or a new one.
func (p *picker) pick(v adapter.Vendor, child string, isNew bool) (bool, Choice) {
	c := p.opts
	c.Vendor, c.Child, c.New = v, child, isNew
	if isNew {
		c.Child = ""
	}
	return true, c
}

// editKeys edits the model name: printable ASCII appends, Backspace
// deletes, Enter sets it once adapter.ValidModel accepts it, and Esc
// leaves it as it was. The name goes to the CLI's argv, so nothing else
// is taken.
func (p *picker) editKeys(b []byte) {
	for _, ch := range b {
		switch {
		case ch == esc:
			p.editing, p.edit, p.editErr = false, nil, ""
			return
		case ch == '\r':
			if err := adapter.ValidModel(string(p.edit)); err != nil {
				p.editErr = "Not a model name: letters, digits, and . _ : / [ ] - only."
				continue
			}
			p.opts.Model, p.editing, p.edit, p.editErr = string(p.edit), false, nil, ""
			return
		case ch == 0x7f || ch == 0x08:
			if len(p.edit) > 0 {
				p.edit = p.edit[:len(p.edit)-1]
			}
		case ch > 0x20 && ch < 0x7f && len(p.edit) < maxModel:
			p.edit = append(p.edit, ch)
		}
	}
}
