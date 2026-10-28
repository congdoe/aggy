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
// screen instead, with the cursor saved, and the CLI is made to repaint
// itself afterwards (see session.closePicker).
const (
	altScreenOn  = "\x1b[?1049h\x1b[H\x1b[2J"
	altScreenOff = "\x1b[?1049l"
	saveCursor   = "\x1b7"
	restCursor   = "\x1b8"
)

// picker is the minimal switch menu: the connected vendors in switch.order
// order, a number or Enter picks one, and Esc, Ctrl+C, or the switch key
// cancels.
type picker struct {
	vendors []adapter.Vendor
	key     byte
}

// draw writes the picker: on the alternate screen, or, when the CLI is
// already on it (overCLI), over the top lines of the CLI's screen.
func (p *picker) draw(w io.Writer, overCLI bool) {
	lines := []string{"aggy: switch vendor", ""}
	if len(p.vendors) == 0 {
		lines = append(lines, "  No other connected vendor. `aggy setup` checks them.")
	}
	for i, v := range p.vendors {
		lines = append(lines, fmt.Sprintf("  %d  %s", i+1, v))
	}
	lines = append(lines, "", "Number or Enter to switch, Esc to go back.")
	var b strings.Builder
	if overCLI {
		b.WriteString(saveCursor)
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
// or, when drawn over the CLI, puts the cursor back (the CLI repaints the
// rest).
func (p *picker) erase(w io.Writer, overCLI bool) {
	if overCLI {
		_, _ = io.WriteString(w, restCursor)
		return
	}
	_, _ = io.WriteString(w, altScreenOff)
}

// handle reads one chunk of input. It returns done when the picker should
// close, and the vendor picked, if any.
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
