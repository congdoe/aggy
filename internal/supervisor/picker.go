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
	// waiting is a picked switch waiting for the turn end, shown in the
	// list; Esc there cancels it.
	waiting adapter.Vendor
	// checking: a check of waiting (the policy probe) is still running.
	checking bool
	// confirm is the vendor a confirmation is for; lines explain it.
	confirm adapter.Vendor
	note    bool
	lines   []string
}

// list sets the picker to the vendor list.
func (p *picker) list() { p.confirm, p.note, p.lines = "", false, nil }

// ask sets the picker to confirm a switch to v, explained by lines.
func (p *picker) ask(v adapter.Vendor, lines ...string) { p.confirm, p.note, p.lines = v, false, lines }

// tell sets the picker to show a note.
func (p *picker) tell(lines ...string) { p.confirm, p.note, p.lines = "", true, lines }

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
	default:
		lines = []string{"aggy: switch vendor", ""}
		if len(p.vendors) == 0 {
			lines = append(lines, "  No other connected vendor. `aggy setup` checks them.")
		}
		for i, v := range p.vendors {
			lines = append(lines, fmt.Sprintf("  %d  %s", i+1, v))
		}
		if p.waiting != "" {
			lines = append(lines, "", fmt.Sprintf("Switching to %s when this turn ends; Esc cancels it, picking it again says no turn is running.", p.waiting))
			if p.checking {
				lines = append(lines, fmt.Sprintf("Checking that %s can get the safety policy...", p.waiting))
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

// handle reads one chunk of input. It returns done when the picker should
// close, and the vendor picked, if any. It reads the list only; a
// confirmation or note is session.promptInput's.
func (p *picker) handle(b []byte) (done bool, picked adapter.Vendor) {
	if len(b) == 1 && b[0] == esc {
		return true, ""
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == esc {
			r, ok := parseCSIu(b[i:])
			if !ok {
				return false, "" // another escape sequence: ignore the rest
			}
			i += r.n - 1
			switch {
			case r.code == 27 || r.code == 'c' && r.mods == 5 || r.code == kittyCode(p.key) && r.mods == 5:
				return true, ""
			case r.code == 13 || r.code == 57414: // Enter, keypad Enter
				c = '\r'
			case r.code >= '1' && r.code <= '9':
				c = byte(r.code)
			default:
				continue
			}
		}
		switch {
		case c == 0x03 || c == p.key:
			return true, ""
		case c == '\r' && len(p.vendors) > 0:
			return true, p.vendors[0]
		case c >= '1' && c <= '9' && int(c-'0') <= len(p.vendors):
			return true, p.vendors[c-'1']
		}
	}
	return false, ""
}
