package trustlist

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// selectOf is a select over claude, OpenCode (a warning), the free models (a
// warning), and the sandbox setting.
func selectOf() *Select {
	return NewSelect(NewState([]Row{claude, oc, free, sandbox}, Folder{}, false, nil))
}

func rn(r rune) Input { return Input{Key: KeyRune, Rune: r} }

var (
	up    = Input{Key: KeyUp}
	down  = Input{Key: KeyDown}
	enter = Input{Key: KeyEnter}
	escK  = Input{Key: KeyEsc}
	back  = Input{Key: KeyBack}
	ctrlC = Input{Key: KeyInterrupt}
)

// TestSelectPress (T6981): every key at the select, from its start.
func TestSelectPress(t *testing.T) {
	cases := []struct {
		name   string
		keys   []Input
		ticked []string
		at     int
		last   Choice
	}{
		{"Enter at once confirms nothing ticked", []Input{enter}, nil, 1, Confirm},
		{"Space at once ticks the harmless row", []Input{rn(' ')}, []string{"claude"}, 1, Toggled},
		{"Down and Space", []Input{down, rn(' ')}, []string{"opencode"}, 2, Toggled},
		{"Up to All and Space ticks the harnesses", []Input{up, rn(' ')}, []string{"claude", "opencode"}, 0, Toggled},
		{"Up stops at All", []Input{up, up, up}, nil, 0, Toggled},
		{"Down stops at the last row", []Input{down, down, down, down, down, down}, nil, 4, Toggled},
		{"j and k move", []Input{rn('j'), rn('j'), rn('k'), rn(' ')}, []string{"opencode"}, 2, Toggled},
		{"Space twice unticks", []Input{rn(' '), rn(' ')}, nil, 1, Toggled},
		{"a digit jumps and ticks", []Input{rn('4')}, []string{"opencode-free"}, 3, Toggled},
		{"1 is All", []Input{rn('1')}, []string{"claude", "opencode"}, 0, Toggled},
		{"a digit not on the list", []Input{rn('9')}, nil, 1, Nothing},
		{"0 is nothing", []Input{rn('0')}, nil, 1, Nothing},
		{"n is not now", []Input{rn(' '), rn('n')}, []string{"claude"}, 1, NotNow},
		{"q is not now", []Input{rn('q')}, nil, 1, NotNow},
		{"Esc is not now", []Input{escK}, nil, 1, NotNow},
		{"x is never", []Input{rn('x')}, nil, 1, Never},
		{"Ctrl-C", []Input{ctrlC}, nil, 1, Interrupt},
		{"another letter does nothing", []Input{rn('a')}, nil, 1, Nothing},
		{"Backspace with nothing typed does nothing", []Input{back}, nil, 1, Nothing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := selectOf()
			var last Choice
			for _, k := range c.keys {
				last = s.Press(k)
			}
			if got := s.St.Ticked(); !slices.Equal(got, c.ticked) {
				t.Errorf("ticked %q, want %q", got, c.ticked)
			}
			if s.At != c.at {
				t.Errorf("at %d, want %d", s.At, c.at)
			}
			if last != c.last {
				t.Errorf("last choice %d, want %d", last, c.last)
			}
		})
	}
}

// TestSelectStart (T6981): the cursor starts on the first row with no
// warning, never on All, so a stray Space or Enter grants nothing risky; with
// every row warned it starts on none, and Space does nothing until a move.
func TestSelectStart(t *testing.T) {
	if s := selectOf(); s.At != 1 {
		t.Errorf("at %d, want 1 (Claude Code)", s.At)
	}
	s := NewSelect(NewState([]Row{oc, free, claude}, Folder{}, false, nil))
	// Shown as OpenCode, Claude Code, then the free models: past OpenCode.
	if s.At != 2 || s.St.Rows[1].ID != "claude" {
		t.Errorf("at %d on %q", s.At, s.St.Rows[max(s.At-1, 0)].ID)
	}
	s = NewSelect(NewState([]Row{oc, free}, Folder{}, false, nil))
	if s.At != -1 {
		t.Fatalf("every row warned: at %d, want none", s.At)
	}
	if c := s.Press(rn(' ')); c != Nothing || len(s.St.Ticked()) != 0 {
		t.Errorf("Space with no cursor: %d, ticked %q", c, s.St.Ticked())
	}
	if strings.Contains(strings.Join(s.Lines(), "\n"), ">") {
		t.Errorf("a cursor is drawn:\n%s", strings.Join(s.Lines(), "\n"))
	}
	s.Press(down)
	if s.At != 0 {
		t.Errorf("the first move: at %d, want All", s.At)
	}
}

// TestSelectDigits (T6981): on a list of ten items or more, a digit that a
// second digit could make a row's number waits for it; Space or Enter ticks
// what was typed (Enter then does not confirm); Backspace takes a digit back;
// any other key drops it.
func TestSelectDigits(t *testing.T) {
	var rows []Row
	for i := range 11 { // items 1 (All) to 12
		rows = append(rows, Row{ID: fmt.Sprintf("r%d", i+2), Label: fmt.Sprintf("row %d", i+2), Group: Source})
	}
	cases := []struct {
		name   string
		keys   []Input
		ticked []string
		last   Choice
	}{
		{"1 then 2 is 12", []Input{rn('1'), rn('2')}, []string{"r12"}, Toggled},
		{"1 waits", []Input{rn('1')}, nil, Toggled},
		{"1 then Space is All (no harness here)", []Input{rn('1'), rn(' ')}, nil, Toggled},
		{"1 then Enter ticks, does not confirm", []Input{rn('1'), rn('1'), rn('1'), enter}, []string{"r11"}, Toggled},
		{"3 at once", []Input{rn('3')}, []string{"r3"}, Toggled},
		{"1 then 9 is not on the list", []Input{rn('1'), rn('9')}, nil, Nothing},
		{"Backspace takes it back", []Input{rn('1'), back, rn('5')}, []string{"r5"}, Toggled},
		{"Esc drops it and is not now", []Input{rn('1'), escK}, nil, NotNow},
		// The cursor starts on r2; the move takes it to r3, and Space ticks
		// that, not a row the 1 began.
		{"a move drops it", []Input{rn('1'), down, rn(' ')}, []string{"r3"}, Toggled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSelect(NewState(rows, Folder{}, false, nil))
			var last Choice
			for _, k := range c.keys {
				last = s.Press(k)
			}
			got := s.St.Ticked()
			if !slices.Equal(got, c.ticked) || last != c.last {
				t.Errorf("ticked %q last %d, want %q %d", got, last, c.ticked, c.last)
			}
		})
	}
}

// TestSelectFrame (T6981): the select drawn at several sizes: wrapped to the
// width, a window around the cursor when it is too tall, the hint last, and
// plain text.
func TestSelectFrame(t *testing.T) {
	st := NewState([]Row{claude, oc, free}, Folder{}, true, nil)
	st.On[0] = true
	cases := []struct {
		name       string
		cols, rows int
		down       int
		want       string
	}{
		{"wide and tall", 140, 40, 0, `   1 [ ] All harnesses

     Harnesses that may run here
>  2 [x] Claude Code   new
   3 [ ] OpenCode   new
          ! plugins run

     Other places this folder's code may be sent
   4 [ ] OpenCode free models   new
          ! sent to OpenCode

Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x: never for this folder.`},
		{"narrow", 30, 40, 0, `   1 [ ] All harnesses

     Harnesses that may run
     here
>  2 [x] Claude Code   new
   3 [ ] OpenCode   new
          ! plugins run

     Other places this
     folder's code may be
     sent
   4 [ ] OpenCode free models
         new
          ! sent to OpenCode

Up/Down move; Space ticks;
Enter confirms; a number
ticks its row; n or Esc: not
now (asked next start); x:
never for this folder.`},
		{"short, cursor at the top", 140, 9, 0, `   1 [ ] All harnesses

     Harnesses that may run here
>  2 [x] Claude Code   new
   3 [ ] OpenCode   new
          ! plugins run
  ... more below

Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x: never for this folder.`},
		{"short, cursor at the bottom", 140, 9, 2, `  ... more above
   3 [ ] OpenCode   new
          ! plugins run

     Other places this folder's code may be sent
>  4 [ ] OpenCode free models   new
          ! sent to OpenCode

Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x: never for this folder.`},
		{"too small for a window", 140, 3, 0, `>  2 [x] Claude Code   new`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSelect(st)
			for range c.down {
				s.Press(down)
			}
			lines := s.Frame(c.cols, c.rows)
			got := strings.Join(lines, "\n")
			if got != c.want {
				t.Errorf("frame:\n%s\nwant:\n%s", got, c.want)
			}
			if len(lines) > c.rows {
				t.Errorf("%d lines in %d rows", len(lines), c.rows)
			}
			for _, l := range lines {
				if utf8.RuneCountInString(l) > c.cols-1 || strings.ContainsAny(l, "\x1b\r\t") {
					t.Errorf("line %q: wider than %d, or not plain", l, c.cols-1)
				}
			}
		})
	}
}

// TestWrapLongWord: a word wider than the screen is cut, never left wider.
func TestWrapLongWord(t *testing.T) {
	for _, w := range []int{1, 2, 5, 9} {
		for _, l := range wrap("        ! "+strings.Repeat("a", 23)+" b", w) {
			if utf8.RuneCountInString(l) > w {
				t.Errorf("width %d: %q", w, l)
			}
		}
	}
}
