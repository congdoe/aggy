package trustlist

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The list as a select (T6981): a cursor over All and the rows, moved with the
// arrows, ticked with Space, confirmed with Enter, as Claude Code's pickers
// are. The keys and the look are the same at a start (internal/cli, drawn in
// place on the normal screen) and in a session (/aggy trust, on aggy's own
// screen). How keys reach it is the caller's: Decoder reads a terminal's bytes
// as keys, and the supervisor maps its own.

// Key is a key at the select that is not a character.
type Key int

// The keys.
const (
	// KeyRune: the key is a character (Input.Rune).
	KeyRune Key = iota
	KeyUp
	KeyDown
	KeyEnter
	// KeyEsc is not now, as n is.
	KeyEsc
	// KeyBack takes back a digit typed for a row not yet ticked.
	KeyBack
	// KeyInterrupt is Ctrl-C: the select ends and the caller stops, as Ctrl-C
	// at the typed list did.
	KeyInterrupt
)

// Input is one key.
type Input struct {
	Key  Key
	Rune rune
}

// SelectKeys is the select's hint line.
const SelectKeys = "Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x: never for this folder."

// Select is a list being answered with keys: its state, and the cursor.
type Select struct {
	St *State
	// At is the item under the cursor: 0 is All, i+1 is row i, and -1 is
	// none (shown at the first move).
	At int
	// digits is a number typed that a further digit could still make a
	// longer number on the list: it ticks its row on Space, Enter, or once
	// it can grow no longer.
	digits string
}

// NewSelect starts the select on st with the cursor on the first row that
// carries no warning, so a stray Space or Enter right away grants nothing
// risky: not All, which ticks every harness. A list whose rows all carry one
// starts with no cursor, and Space does nothing until a move.
func NewSelect(st *State) *Select {
	s := &Select{St: st, At: -1}
	for i, r := range st.Rows {
		if r.Warning == "" {
			s.At = i + 1
			break
		}
	}
	return s
}

// items is how many items the cursor moves over: All and each row.
func (s *Select) items() int { return len(s.St.Rows) + 1 }

// Press takes one key and says what it did: Toggled when the cursor moved or
// a row was ticked or unticked, Nothing when the key does nothing here, and
// Confirm, NotNow, Never, or Interrupt when it ends the select.
//
// Digits: a digit ticks the row of that number at once (1 is All) and moves
// the cursor there, unless the list is long enough that another digit could
// make a longer number on it (10 and up); then it waits for that digit, and
// Space or Enter ticks the number typed so far (Enter then does not confirm).
// Any other key drops a number waiting.
func (s *Select) Press(in Input) Choice {
	if in.Key == KeyRune && in.Rune >= '0' && in.Rune <= '9' {
		return s.digit(in.Rune)
	}
	if s.digits != "" {
		switch {
		case in.Key == KeyRune && in.Rune == ' ', in.Key == KeyEnter:
			return s.typed()
		case in.Key == KeyBack:
			s.digits = s.digits[:len(s.digits)-1]
			return Toggled
		}
		s.digits = ""
	}
	switch in.Key {
	case KeyUp:
		s.move(-1)
		return Toggled
	case KeyDown:
		s.move(1)
		return Toggled
	case KeyEnter:
		return Confirm
	case KeyEsc:
		return NotNow
	case KeyInterrupt:
		return Interrupt
	case KeyRune:
		switch in.Rune {
		case 'k':
			s.move(-1)
			return Toggled
		case 'j':
			s.move(1)
			return Toggled
		case ' ':
			if s.At < 0 {
				return Nothing
			}
			s.St.Toggle(s.At + 1)
			return Toggled
		case 'n', 'N', 'q':
			return NotNow
		case 'x', 'X':
			return Never
		}
	}
	return Nothing
}

// move moves the cursor by d, staying on the list; with none, a move shows
// it on All.
func (s *Select) move(d int) {
	if s.At < 0 {
		s.At = 0
		return
	}
	s.At = max(0, min(s.At+d, s.items()-1))
}

// digit takes one typed digit.
func (s *Select) digit(r rune) Choice {
	d := s.digits + string(r)
	n := atoi(d)
	if n < 1 || n > s.items() {
		s.digits = ""
		return Nothing
	}
	if n*10 <= s.items() {
		s.digits = d // a further digit could still name a row
		return Toggled
	}
	s.digits = d
	return s.typed()
}

// typed ticks the row the digits name, and moves the cursor there.
func (s *Select) typed() Choice {
	n := atoi(s.digits)
	s.digits = ""
	if !s.St.Toggle(n) {
		return Nothing
	}
	s.At = n - 1
	return Toggled
}

func atoi(d string) int {
	n := 0
	for _, c := range d {
		n = n*10 + int(c-'0')
	}
	return n
}

// entry is one line of the list, and the item it belongs to (-1 for a
// group's title or a blank line).
type entry struct {
	text string
	item int
}

// entries are the list's lines: All, then each group's rows with their
// numbers, ticks, new marks, and warnings.
func (s *State) entries() []entry {
	box := func(on bool) string {
		if on {
			return "[x]"
		}
		return "[ ]"
	}
	out := []entry{{fmt.Sprintf(" 1 %s All harnesses", box(s.AllOn())), 0}}
	var g Group
	for i, r := range s.Rows {
		if r.Group != g {
			g = r.Group
			out = append(out, entry{"", -1}, entry{"   " + g.Title(), -1})
		}
		label := r.Label
		if r.Missing {
			label += " (not installed)"
		}
		if s.New[i] {
			label += "   new"
		}
		out = append(out, entry{fmt.Sprintf("%2d %s %s", i+2, box(s.On[i]), label), i + 1})
		if r.Warning != "" {
			out = append(out, entry{"        ! " + r.Warning, i + 1})
		}
	}
	return out
}

// Lines are the list with the cursor: "> " before the item under it, two
// spaces before every other line. Every warning shows, the highlighted row's
// among them.
func (s *Select) Lines() []string {
	var out []string
	for _, e := range s.St.entries() {
		mark := "  "
		if e.item == s.At && s.At >= 0 && !strings.HasPrefix(e.text, "        !") {
			mark = "> "
		}
		out = append(out, strings.TrimRight(mark+e.text, " "))
	}
	return out
}

// Frame is the select drawn in cols columns and at most rows rows: its lines
// wrapped to fit (a terminal must not wrap them, or a redraw in place would
// miscount), a window around the cursor with a line saying what is above or
// below when they do not all fit, and the hint last. It is plain text. A
// screen too small for a window shows the cursor's line alone.
func (s *Select) Frame(cols, rows int) []string {
	width := max(cols-1, 1) // the last column is never written
	var phys []string
	first, last := -1, -1
	lines := s.Lines()
	for i, e := range s.St.entries() {
		for _, w := range wrap(lines[i], width) {
			if e.item == s.At && s.At >= 0 {
				if first < 0 {
					first = len(phys)
				}
				last = len(phys)
			}
			phys = append(phys, w)
		}
	}
	hint := SelectKeys
	if s.digits != "" {
		hint = "Typed " + s.digits + ": another digit, or Space or Enter to tick it. " + hint
	}
	foot := append([]string{""}, wrap(hint, width)...)
	if len(phys)+len(foot) <= rows {
		return append(phys, foot...)
	}
	room := rows - len(foot)
	if room < 3 {
		// No room for a window: the cursor's line (or the first) alone.
		at := max(first, 0)
		return []string{cut(phys[at], width)}
	}
	if first < 0 {
		first, last = 0, 0
	}
	start := 0
	var end int
	for {
		body := room
		if start > 0 {
			body-- // the line saying more is above
		}
		end = min(start+body, len(phys))
		if end < len(phys) {
			end-- // the line saying more is below
		}
		if last < end || start >= first {
			break
		}
		start++
	}
	var out []string
	if start > 0 {
		out = append(out, cut("  ... more above", width))
	}
	out = append(out, phys[start:end]...)
	if end < len(phys) {
		out = append(out, cut("  ... more below", width))
	}
	return append(out, foot...)
}

// wrap breaks line at spaces into lines of at most width runes; a line
// carried on is indented as the line's text starts (after its spaces, the
// cursor's mark, and a warning's !). A word longer than width is cut.
func wrap(line string, width int) []string {
	if utf8.RuneCountInString(line) <= width {
		return []string{line}
	}
	indent := len(line) - len(strings.TrimLeft(line, " >!"))
	// A row's label carries on under itself, past its number and box.
	rest := strings.TrimLeft(line[indent:], "0123456789")
	if len(rest) < len(line)-indent && (strings.HasPrefix(rest, " [ ] ") || strings.HasPrefix(rest, " [x] ")) {
		indent = len(line) - len(rest) + len(" [ ] ")
	}
	if indent > width/2 {
		indent = 0
	}
	pad := strings.Repeat(" ", indent)
	var out []string
	cur, empty := line[:indent], true
	for _, w := range strings.Fields(line[indent:]) {
		if !empty && utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) > width {
			out = append(out, cur)
			cur, empty = pad, true
		}
		if !empty {
			cur += " "
		}
		cur, empty = cur+w, false
		for utf8.RuneCountInString(cur) > width {
			r := []rune(cur)
			out = append(out, string(r[:width]))
			cur = pad + string(r[width:])
		}
	}
	if !empty {
		out = append(out, cur)
	}
	return out
}

// cut shortens s to width runes.
func cut(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	return string([]rune(s)[:width])
}
