//go:build unix

package supervisor

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Option lists and results on the alternate screen (T6909). A command with its
// arguments left out lists its options (menu), and a command that starts no
// turn shows what it printed (page). Both take the whole screen, as the picker
// does, hold the CLI's output while open, and Esc returns to the CLI, so
// nothing enters the conversation.

// menuItem is one option: a command line, as typed, that picking runs, or,
// with fill, the start of one that picking types into the composer for the
// person to finish.
type menuItem struct {
	label string
	line  string
	fill  bool
}

// menu is an option list.
type menu struct {
	title string
	items []menuItem
	at    int // the highlighted item
}

// page is a result: lines to scroll, from top. confirm, when set, runs on y:
// the arguments, what the key does, and whether that run answers its own
// question y. busy: a run is going, and its result replaces lines.
type page struct {
	title        string
	lines        []string
	top          int
	confirm      []string
	confirmLabel string
	answer       bool
	busy         bool
	seq          int
}

// screenRows and screenCols are the size the menu and page draw at when the
// terminal reports none.
const (
	screenRows = 24
	screenCols = 80
)

// drawView writes the menu or the page over the whole screen: on the
// alternate screen, or over the CLI when it is on it already (overCLI), every
// row written, so none of the CLI's shows through.
func (p *picker) drawView(w io.Writer, overCLI bool, rows, cols int) {
	if rows < 3 {
		rows = screenRows
	}
	if cols < 10 {
		cols = screenCols
	}
	var lines []string
	switch {
	case p.menu != nil:
		m := p.menu
		lines = append(lines, "aggy: "+m.title, "")
		room := rows - 4
		start := 0
		if m.at >= room {
			start = m.at - room + 1
		}
		for i := start; i < len(m.items) && i < start+room; i++ {
			mark := " "
			if i == m.at {
				mark = ">"
			}
			num := "  "
			if i < 9 {
				num = fmt.Sprintf("%d ", i+1)
			}
			lines = append(lines, fmt.Sprintf(" %s%s %s", mark, num, m.items[i].label))
		}
		for len(lines) < rows-2 {
			lines = append(lines, "")
		}
		lines = append(lines, "", "Up, Down, and Enter or a number pick; Esc goes back to the CLI.")
	case p.page != nil:
		g := p.page
		lines = append(lines, "aggy: "+g.title, "")
		room := rows - 4
		g.top = max(min(g.top, len(g.lines)-room), 0)
		for i := g.top; i < len(g.lines) && i < g.top+room; i++ {
			lines = append(lines, "  "+g.lines[i])
		}
		for len(lines) < rows-2 {
			lines = append(lines, "")
		}
		foot := "Esc goes back to the CLI."
		if len(g.lines) > room {
			foot = "Up, Down, Space, and b scroll; " + foot
		}
		switch {
		case g.busy:
			foot = "Running... " + foot
		case len(g.confirm) > 0:
			foot = g.confirmLabel + "; " + foot
		}
		lines = append(lines, "", foot)
	}
	var b strings.Builder
	if overCLI {
		for i, l := range lines {
			fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K%s", i+1, cut(l, cols-1))
		}
	} else {
		if p.onAlt {
			b.WriteString(altScreenClear)
		} else {
			b.WriteString(altScreenOn)
		}
		p.onAlt = true
		for i, l := range lines {
			b.WriteString(cut(l, cols-1))
			if i < len(lines)-1 {
				b.WriteString("\r\n")
			}
		}
	}
	_, _ = io.WriteString(w, b.String())
}

// cut shortens s to n runes; it never writes the last column.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:max(n, 0)])
}

// plainText is what a command printed, made safe to draw: lines, with any
// escape sequence and control character but Tab taken out, and Tab as spaces.
func plainText(s string) []string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == esc:
			i += seqLen([]byte(s[i:])) - 1
		case c == '\n':
			b.WriteByte('\n')
		case c == '\t':
			b.WriteString("    ")
		case c < 0x20 || c == 0x7f:
		default:
			b.WriteByte(c)
		}
	}
	out := strings.Split(strings.TrimRight(strings.ToValidUTF8(b.String(), "?"), "\n"), "\n")
	for i, l := range out {
		out[i] = strings.Map(func(r rune) rune {
			if r >= 0x80 && r < 0xa0 { // C1 controls
				return -1
			}
			return r
		}, l)
	}
	return out
}

// viewKey is one key of a menu or page.
type viewKey int

const (
	keyNone viewKey = iota
	keyUp
	keyDown
	keyPageUp
	keyPageDown
	keyEnter
	keyClose
)

// viewKeys reads b as keys, outside a paste (pastes are dropped before this):
// it returns each key, and each digit or letter as a rune.
func viewKeys(b []byte, swKey byte) (keys []viewKey, runes []rune) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		k, r := keyNone, rune(0)
		switch {
		case c == esc && i+1 == len(b):
			k = keyClose
		case c == esc:
			n := seqLen(b[i:])
			seq := string(b[i : i+n])
			i += n - 1
			switch seq {
			case "\x1b[A", "\x1bOA":
				k = keyUp
			case "\x1b[B", "\x1bOB":
				k = keyDown
			case "\x1b[5~":
				k = keyPageUp
			case "\x1b[6~":
				k = keyPageDown
			default:
				if cu, ok := parseCSIu([]byte(seq)); ok && cu.event != 3 {
					mods := (cu.mods - 1) &^ modLocks
					switch {
					case cu.code == 27 || mods == modCtrl && (cu.code == 'c' || cu.code == kittyCode(swKey)):
						k = keyClose
					case cu.code == 13 || cu.code == kittyKeypadCR:
						k = keyEnter
					case mods == 0 && cu.code > 0x20 && cu.code < 0x7f:
						r = rune(cu.code)
					}
				}
			}
		case c == 0x03 || c == swKey:
			k = keyClose
		case c == '\r':
			k = keyEnter
		case c > 0x20 && c < 0x7f:
			r = rune(c)
		case c == ' ':
			r = ' '
		}
		keys, runes = append(keys, k), append(runes, r)
	}
	return keys, runes
}

// pasteFilter drops bracketed pastes from input to a menu or a page, the
// markers too, even split across reads.
type pasteFilter struct {
	in   bool
	tail []byte
}

// drop returns b without what is pasted.
func (f *pasteFilter) drop(b []byte) []byte {
	if len(f.tail) > 0 {
		b = append(f.tail, b...)
		f.tail = nil
	}
	var out []byte
	for i := 0; i < len(b); {
		m := pasteStart
		if f.in {
			m = pasteEnd
		}
		rest := b[i:]
		if rest[0] == esc && len(rest) < len(m) && strings.HasPrefix(string(m), string(rest)) && len(rest) > 1 {
			f.tail = append([]byte(nil), rest...)
			return out
		}
		switch {
		case strings.HasPrefix(string(rest), string(pasteStart)):
			f.in = true
			i += len(pasteStart)
		case strings.HasPrefix(string(rest), string(pasteEnd)):
			f.in = false
			i += len(pasteEnd)
		case f.in:
			i++
		default:
			out = append(out, rest[0])
			i++
		}
	}
	return out
}
