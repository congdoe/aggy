package trustlist

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// The list as a select (T6981): a cursor over All and the rows, moved with the
// arrows, ticked with Space, confirmed with Enter, as Claude Code's pickers
// are. The keys and the look are the same at a start (internal/cli, drawn in
// place on the normal screen) and in a session (/aggy trust, on aggy's own
// screen). How keys reach it is the caller's: Decoder reads a terminal's bytes
// as keys, and the supervisor maps its own.
//
// Text typed at it is not an answer (PR #140 review): a person who runs aggy
// and starts typing their first prompt, as at claude, may see the list appear
// mid-word. So a key with no meaning here, or a character within typingRun of
// another, means typing: what that run of characters did is undone, and every
// key is ignored until a pause of typingQuiet. Enter confirms only right after
// a move or a tick, or as the first key, enterQuiet after the list appeared;
// x (never, which is recorded) needs Enter or y after it. n, q, and Esc, which
// record nothing, stay one key. Ctrl-C always ends it.

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
	// KeyUnknown is input that is no key here: a sequence aggy does not know,
	// a paste, or a burst of keys in one read. It means typing.
	KeyUnknown
)

// Input is one key.
type Input struct {
	Key  Key
	Rune rune
}

// SelectKeys is the select's hint line.
const SelectKeys = "Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x then Enter: never for this folder."

// The typing guard's times.
const (
	// typingRun: a character this soon after another is typing.
	typingRun = 300 * time.Millisecond
	// typingQuiet: typing is over once no key comes for this long.
	typingQuiet = time.Second
	// enterQuiet: Enter as the first key confirms only this long after the
	// list appeared.
	enterQuiet = 500 * time.Millisecond
)

// TypingNote is the line the select shows while it ignores typed text.
const TypingNote = "Ignoring typed text: pause, then use the arrows."

// NeverNote is the line the select shows after x.
const NeverNote = "Never ask in this folder? Enter or y says never; any other key goes back to the list."

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
	// typing: keys are ignored until a pause (the package's comment).
	typing bool
	// never: x was pressed; Enter or y says never.
	never bool
	// acted: the last key moved the cursor or ticked a row, so Enter may
	// confirm.
	acted bool
	// keys counts the keys taken since the list appeared.
	keys int
	// last is when the last key came, or the list appeared; lastChar when
	// the last character (or input that is no key) came.
	last, lastChar time.Time
	// before is the list as it was before the current run of characters,
	// put back when the run turns out to be typing.
	beforeOn []bool
	beforeAt int
}

// NewSelect starts the select on st with the cursor on a row a stray Space
// right away cannot make risky: never All, which ticks every harness. On a
// list asked again for new rows, the first new row with no warning, else the
// first new row, so a Space never changes an earlier answer unseen; otherwise
// the first row with no warning. A first list whose rows all carry a warning
// starts with no cursor, and Space does nothing until a move.
func NewSelect(st *State) *Select {
	s := &Select{St: st, At: -1}
	pick := func(ok func(i int) bool) bool {
		for i := range st.Rows {
			if ok(i) {
				s.At = i + 1
				return true
			}
		}
		return false
	}
	if slices.Contains(st.New, true) {
		_ = pick(func(i int) bool { return st.New[i] && st.Rows[i].Warning == "" }) || pick(func(i int) bool { return st.New[i] })
	} else {
		pick(func(i int) bool { return st.Rows[i].Warning == "" })
	}
	s.mark()
	return s
}

// Shown records when the list appeared, for Enter as the first key.
func (s *Select) Shown(now time.Time) { s.last = now }

// Typed takes a key that came within the type-ahead window, typed before
// the list was there: it answers nothing, and the select treats what follows
// as typing until a pause.
func (s *Select) Typed(now time.Time) {
	s.last, s.lastChar = now, now
	s.startTyping()
}

// Typing reports whether the select is ignoring typed text.
func (s *Select) Typing() bool { return s.typing }

// mark keeps the list as it is now, to put back if a run of characters turns
// out to be typing.
func (s *Select) mark() {
	s.beforeOn, s.beforeAt = slices.Clone(s.St.On), s.At
}

// startTyping puts back what the run of characters did and ignores keys
// until a pause.
func (s *Select) startTyping() Choice {
	copy(s.St.On, s.beforeOn)
	s.At = s.beforeAt
	s.typing, s.never, s.acted, s.digits = true, false, false, ""
	return Toggled
}

// items is how many items the cursor moves over: All and each row.
func (s *Select) items() int { return len(s.St.Rows) + 1 }

// Press takes one key at now and says what it did: Toggled when the cursor
// moved, a row was ticked or unticked, or what the select shows changed;
// Nothing when the key does nothing here; and Confirm, NotNow, Never, or
// Interrupt when it ends the select.
//
// Digits: a digit ticks the row of that number at once (1 is All) and moves
// the cursor there, unless the list is long enough that another digit could
// make a longer number on it (10 and up); then it waits for that digit, and
// Space or Enter ticks the number typed so far (Enter then does not confirm).
// Any other key drops a number waiting.
func (s *Select) Press(in Input, now time.Time) Choice {
	if in.Key == KeyInterrupt {
		return Interrupt
	}
	gap := now.Sub(s.last)
	s.last = now
	s.keys++
	if s.typing {
		if gap < typingQuiet {
			return Nothing
		}
		s.typing = false
		s.mark()
	}
	if in.Key == KeyRune || in.Key == KeyUnknown {
		run := !s.lastChar.IsZero() && now.Sub(s.lastChar) < typingRun
		s.lastChar = now
		// A digit after a digit waiting is one number, typed as fast as the
		// person likes.
		number := s.digits != "" && in.Key == KeyRune && in.Rune >= '0' && in.Rune <= '9'
		if run && !number {
			return s.startTyping()
		}
		if !run {
			s.mark()
		}
	}
	acted := s.acted
	s.acted = false
	if s.never {
		s.never = false
		if (in.Key == KeyEnter || in.Key == KeyRune && (in.Rune == 'y' || in.Rune == 'Y')) && gap >= typingRun {
			return Never
		}
		if in.Key == KeyEnter || in.Key == KeyRune && (in.Rune == 'y' || in.Rune == 'Y') {
			return Toggled // too soon after x to be an answer: back to the list
		}
		// Any other key goes back to the list, and does what it does there.
	}
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
		return s.move(-1)
	case KeyDown:
		return s.move(1)
	case KeyEnter:
		if acted || s.keys == 1 && gap >= enterQuiet {
			return Confirm
		}
		return Nothing
	case KeyEsc:
		return NotNow
	case KeyBack:
		return Nothing
	case KeyRune:
		switch in.Rune {
		case 'k':
			return s.move(-1)
		case 'j':
			return s.move(1)
		case ' ':
			if s.At < 0 {
				return Nothing
			}
			s.St.Toggle(s.At + 1)
			s.acted = true
			return Toggled
		case 'n', 'N', 'q':
			return NotNow
		case 'x', 'X':
			s.never = true
			return Toggled
		}
	}
	// A character with no meaning here, or input that is no key: typing.
	return s.startTyping()
}

// move moves the cursor by d, staying on the list; with none, a move shows
// it on All.
func (s *Select) move(d int) Choice {
	s.acted = true
	if s.At < 0 {
		s.At = 0
		return Toggled
	}
	s.At = max(0, min(s.At+d, s.items()-1))
	return Toggled
}

// digit takes one typed digit; one that names no row on the list is typing.
func (s *Select) digit(r rune) Choice {
	d := s.digits + string(r)
	n := atoi(d)
	if n < 1 || n > s.items() {
		return s.startTyping()
	}
	s.digits = d
	if n*10 <= s.items() {
		return Toggled // a further digit could still name a row
	}
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
	s.acted = true
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
	switch {
	case s.typing:
		hint = TypingNote + " " + hint
	case s.never:
		hint = NeverNote
	case s.digits != "":
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
// TODO(T6954): it counts runes, so a wide character (two columns) would
// misdraw; every label today is ASCII.
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
