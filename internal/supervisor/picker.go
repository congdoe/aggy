package supervisor

import (
	"fmt"
	"io"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// The picker is drawn on the terminal's alternate screen, so leaving it
// puts back the vendor's screen and cursor exactly, without aggy redrawing
// anything. The CLIs run inline (Claude) or with --no-alt-screen, so the
// alternate screen is free.
const (
	altScreenOn  = "\x1b[?1049h\x1b[H\x1b[2J"
	altScreenOff = "\x1b[?1049l"
)

// picker is the minimal switch menu: the connected vendors in switch.order
// order, a number or Enter picks one, and Esc, Ctrl+C, or the switch key
// cancels.
type picker struct {
	vendors []adapter.Vendor
	key     byte
}

// draw writes the picker on the alternate screen.
func (p *picker) draw(w io.Writer) {
	var b strings.Builder
	b.WriteString(altScreenOn)
	b.WriteString("aggy: switch vendor\r\n\r\n")
	if len(p.vendors) == 0 {
		b.WriteString("  No other connected vendor. `aggy setup` checks them.\r\n")
	}
	for i, v := range p.vendors {
		fmt.Fprintf(&b, "  %d  %s\r\n", i+1, v)
	}
	b.WriteString("\r\nNumber or Enter to switch, Esc to go back.\r\n")
	_, _ = io.WriteString(w, b.String())
}

// erase leaves the alternate screen, restoring the vendor's screen.
func (p *picker) erase(w io.Writer) { _, _ = io.WriteString(w, altScreenOff) }

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
