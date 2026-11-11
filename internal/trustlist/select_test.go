package trustlist

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
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
	unkn  = Input{Key: KeyUnknown}
)

// t0 is when a test's list appeared.
var t0 = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// pressAll presses keys gap apart, the first gap after the list appeared,
// and returns the last choice.
func pressAll(s *Select, gap time.Duration, keys ...Input) Choice {
	s.Shown(t0)
	var last Choice
	for i, k := range keys {
		last = s.Press(k, t0.Add(time.Duration(i+1)*gap))
	}
	return last
}

// TestSelectPress (T6981): every key at the select, from its start, pressed
// a second apart, as a person answering does.
func TestSelectPress(t *testing.T) {
	cases := []struct {
		name   string
		keys   []Input
		ticked []string
		at     int
		last   Choice
	}{
		{"Enter as the first key confirms nothing ticked", []Input{enter}, nil, 1, Confirm},
		{"Space at once ticks the harmless row", []Input{rn(' ')}, []string{"claude"}, 1, Toggled},
		{"Down and Space", []Input{down, rn(' ')}, []string{"opencode"}, 2, Toggled},
		{"Up to All and Space ticks the harnesses", []Input{up, rn(' ')}, []string{"claude", "opencode"}, 0, Toggled},
		{"Up stops at All", []Input{up, up, up}, nil, 0, Toggled},
		{"Down stops at the last row", []Input{down, down, down, down, down, down}, nil, 4, Toggled},
		{"j and k move", []Input{rn('j'), rn('j'), rn('k'), rn(' ')}, []string{"opencode"}, 2, Toggled},
		{"Space twice unticks", []Input{rn(' '), rn(' ')}, nil, 1, Toggled},
		{"Enter after a tick confirms", []Input{rn(' '), enter}, []string{"claude"}, 1, Confirm},
		{"Enter after a move confirms", []Input{down, enter}, nil, 2, Confirm},
		{"a digit jumps and ticks", []Input{rn('4')}, []string{"opencode-free"}, 3, Toggled},
		{"1 is All", []Input{rn('1')}, []string{"claude", "opencode"}, 0, Toggled},
		{"Enter after a digit confirms", []Input{rn('2'), enter}, []string{"claude"}, 1, Confirm},
		{"a digit not on the list is typing", []Input{rn('9')}, nil, 1, Toggled},
		{"0 is typing", []Input{rn('0')}, nil, 1, Toggled},
		{"n is not now", []Input{rn(' '), rn('n')}, []string{"claude"}, 1, NotNow},
		{"q is not now", []Input{rn('q')}, nil, 1, NotNow},
		{"Esc is not now", []Input{escK}, nil, 1, NotNow},
		{"x alone asks", []Input{rn('x')}, nil, 1, Toggled},
		{"x then Enter is never", []Input{rn('x'), enter}, nil, 1, Never},
		{"x then y is never", []Input{rn('x'), rn('y')}, nil, 1, Never},
		{"x then another key goes back", []Input{rn('x'), down, enter}, nil, 2, Confirm},
		{"Ctrl-C", []Input{ctrlC}, nil, 1, Interrupt},
		{"another letter is typing", []Input{rn('a')}, nil, 1, Toggled},
		{"Backspace with nothing typed does nothing", []Input{back}, nil, 1, Nothing},
		{"Enter after a key that did nothing does not confirm", []Input{back, enter}, nil, 1, Nothing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := selectOf()
			last := pressAll(s, time.Second, c.keys...)
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

// TestSelectTyping (PR #140 review, H1): text typed at the list, a byte a
// read at typing speed, as a person typing their first prompt does when the
// list appears mid-word, never ticks, confirms, or says never. Each line
// fails at 4de725e, where a key per read was acted on.
func TestSelectTyping(t *testing.T) {
	cases := []struct {
		name string
		text string
		gap  time.Duration
	}{
		{"ok go", "ok go\r", 80 * time.Millisecond},
		{"see issue 4", "see issue 4\r", 80 * time.Millisecond},
		{"add tests", "add tests\r", 80 * time.Millisecond},
		{"fix it", "fix it\r", 80 * time.Millisecond},
		{"a word that starts with a move", "just do it\r", 80 * time.Millisecond},
		{"a leading space", " hello\r", 80 * time.Millisecond},
		{"a leading digit", "2 files\r", 80 * time.Millisecond},
		{"x and a word", "x marks\r", 80 * time.Millisecond},
		{"x and a fast Enter", "x\r", 80 * time.Millisecond},
		{"slow typing", "ok go\r", 250 * time.Millisecond},
		// PR #140 re-review, M1: a slow typist's characters, each past the
		// 300 ms run, tick on the way (k to All, Space ticks it; 1 ticks All);
		// typing undoes them all. Each fails at 9219f2d.
		{"slow, a move and a tick first", "k the\r", 400 * time.Millisecond},
		{"slow, digits first", "12 tests\r", 400 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := selectOf()
			s.Shown(t0)
			var d Decoder
			at := t0.Add(time.Second)
			for _, b := range []byte(c.text) {
				for _, k := range d.Feed([]byte{b}) {
					switch got := s.Press(k, at); got {
					case Confirm, Never, Interrupt:
						t.Fatalf("%q answered %d", b, got)
					}
				}
				at = at.Add(c.gap)
			}
			if len(s.St.Ticked()) != 0 || s.never || s.At != 1 {
				t.Errorf("ticked %q, never %v, at %d", s.St.Ticked(), s.never, s.At)
			}
			// The person pauses and hits Enter to send their prompt: still no
			// answer. A move after a pause, then Enter, is one.
			at = at.Add(1500 * time.Millisecond)
			if got := s.Press(enter, at); got == Confirm {
				t.Error("Enter after typing and a pause confirmed")
			}
			if s.Typing() {
				t.Error("still typing after a pause")
			}
			// M2: and the list says why, without claiming Enter confirms.
			frame := strings.Join(s.Frame(400, 40), "\n")
			if !strings.Contains(frame, EnterNote) || strings.Contains(frame, "Enter confirms;") {
				t.Errorf("an ignored Enter is not explained:\n%s", frame)
			}
			s.Press(down, at.Add(time.Second))
			if strings.Contains(strings.Join(s.Frame(400, 40), "\n"), EnterNote) {
				t.Error("the note stays after a move")
			}
			if got := s.Press(enter, at.Add(2*time.Second)); got != Confirm {
				t.Errorf("a move and Enter after typing: %d", got)
			}
		})
	}
}

// TestSelectTypingShown: keys typed ahead of the list start the typing guard,
// which the hint says, and a key that is no key starts it too.
func TestSelectTypingShown(t *testing.T) {
	s := selectOf()
	s.Shown(t0)
	s.Typed(t0.Add(50 * time.Millisecond))
	if !strings.Contains(strings.Join(s.Frame(200, 40), "\n"), TypingNote) {
		t.Error("the typing note is not shown")
	}
	if got := s.Press(rn(' '), t0.Add(500*time.Millisecond)); got != Nothing || len(s.St.Ticked()) != 0 {
		t.Errorf("Space while typing: %d %q", got, s.St.Ticked())
	}
	if got := s.Press(ctrlC, t0.Add(600*time.Millisecond)); got != Interrupt {
		t.Errorf("Ctrl-C while typing: %d", got)
	}
	s = selectOf()
	// Typing puts back the list as at the last key that is no character: a
	// tick by Space is undone, one after an arrow and Enter is kept.
	pressAll(s, time.Second, rn(' '), unkn)
	if !s.Typing() || len(s.St.Ticked()) != 0 {
		t.Errorf("an unknown key after a Space: typing %v, ticked %q", s.Typing(), s.St.Ticked())
	}
	s = selectOf()
	pressAll(s, time.Second, down, rn(' '), up, unkn)
	if !s.Typing() || !slices.Equal(s.St.Ticked(), []string{"opencode"}) || s.At != 1 {
		t.Errorf("an unknown key after a tick and an arrow: typing %v, ticked %q, at %d", s.Typing(), s.St.Ticked(), s.At)
	}
	s = selectOf()
	pressAll(s, time.Second, rn('x'))
	if !strings.Contains(strings.Join(s.Frame(200, 40), "\n"), NeverNote) {
		t.Error("x does not ask")
	}
}

// TestSelectStart (T6981): the cursor starts on the first row with no
// warning, never on All, so a stray Space or Enter grants nothing risky; with
// every row warned it starts on none, and Space does nothing until a move.
// Asked again for new rows (PR #140 review, L4), it starts on the first new
// row with no warning, else on none (T6957: a new row with a warning, such as
// Antigravity's terms row, is never one Space away).
func TestSelectStart(t *testing.T) {
	if s := selectOf(); s.At != 1 {
		t.Errorf("at %d, want 1 (Claude Code)", s.At)
	}
	s := NewSelect(NewState([]Row{oc, free, claude}, Folder{}, false, nil))
	// Shown as OpenCode, Claude Code, then the free models: past OpenCode.
	if s.At != 2 || s.St.Rows[1].ID != "claude" {
		t.Errorf("at %d on %q", s.At, s.St.Rows[max(s.At-1, 0)].ID)
	}
	answered := Folder{Shown: []string{"claude", "opencode"}, On: []string{"claude"}}
	s = NewSelect(NewState([]Row{claude, oc, free, sandbox}, answered, true, nil))
	if s.At != 4 || s.St.Rows[3].ID != "claude-sandbox" {
		t.Errorf("re-asked: at %d, want the new sandbox row", s.At)
	}
	s = NewSelect(NewState([]Row{claude, oc, free}, answered, true, nil))
	if s.At != -1 {
		t.Errorf("re-asked, the new row warned: at %d, want none", s.At)
	}
	s = NewSelect(NewState([]Row{oc, free}, Folder{}, false, nil))
	if s.At != -1 {
		t.Fatalf("every row warned: at %d, want none", s.At)
	}
	if c := pressAll(s, time.Second, rn(' ')); c != Nothing || len(s.St.Ticked()) != 0 {
		t.Errorf("Space with no cursor: %d, ticked %q", c, s.St.Ticked())
	}
	if strings.Contains(strings.Join(s.Lines(), "\n"), ">") {
		t.Errorf("a cursor is drawn:\n%s", strings.Join(s.Lines(), "\n"))
	}
	s.Press(down, t0.Add(time.Hour))
	if s.At != 0 {
		t.Errorf("the first move: at %d, want All", s.At)
	}
}

// TestSelectDigits (T6981): on a list of exactly ten items (All and nine
// rows; PR #141 can make it that long), a 1 waits, since 10 is on the list:
// Space or Enter ticks what was typed (Enter then does not confirm, so 1 then
// Enter ticks All); 1 then 0 is row 10, however fast; Backspace takes a digit
// back; any other key drops it.
func TestSelectDigits(t *testing.T) {
	var rows []Row
	for i := range 9 {
		rows = append(rows, Row{ID: fmt.Sprintf("h%d", i+2), Label: fmt.Sprintf("row %d", i+2), Group: Harness})
	}
	all := []string{"h2", "h3", "h4", "h5", "h6", "h7", "h8", "h9", "h10"}
	cases := []struct {
		name   string
		gap    time.Duration
		keys   []Input
		ticked []string
		last   Choice
	}{
		{"1 waits", time.Second, []Input{rn('1')}, nil, Toggled},
		{"1 then Enter ticks All, does not confirm", time.Second, []Input{rn('1'), enter}, all, Toggled},
		{"then Enter confirms", time.Second, []Input{rn('1'), enter, enter}, all, Confirm},
		{"1 then Space ticks All", time.Second, []Input{rn('1'), rn(' ')}, all, Toggled},
		{"1 then 0 is row 10", time.Second, []Input{rn('1'), rn('0')}, []string{"h10"}, Toggled},
		{"1 then 0, fast", 100 * time.Millisecond, []Input{rn('1'), rn('0')}, []string{"h10"}, Toggled},
		{"2 at once", time.Second, []Input{rn('2')}, []string{"h2"}, Toggled},
		{"1 then 1 is not on the list", time.Second, []Input{rn('1'), rn('1')}, nil, Toggled},
		{"Backspace takes it back", time.Second, []Input{rn('1'), back, rn('5')}, []string{"h5"}, Toggled},
		{"Esc drops it and is not now", time.Second, []Input{rn('1'), escK}, nil, NotNow},
		// The cursor starts on h2; the move takes it to h3, and Space ticks
		// that, not a row the 1 began.
		{"a move drops it", time.Second, []Input{rn('1'), down, rn(' ')}, []string{"h3"}, Toggled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSelect(NewState(rows, Folder{}, false, nil))
			if s.items() != 10 {
				t.Fatalf("%d items", s.items())
			}
			last := pressAll(s, c.gap, c.keys...)
			if got := s.St.Ticked(); !slices.Equal(got, c.ticked) || last != c.last {
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
		{"wide and tall", 200, 40, 0, `   1 [ ] All harnesses

     Harnesses that may run here
>  2 [x] Claude Code   new
   3 [ ] OpenCode   new
          ! plugins run

     Other places this folder's code may be sent
   4 [ ] OpenCode free models   new
          ! sent to OpenCode

Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x then Enter: never for this folder.`},
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
now (asked next start); x
then Enter: never for this
folder.`},
		{"short, cursor at the top", 200, 9, 0, `   1 [ ] All harnesses

     Harnesses that may run here
>  2 [x] Claude Code   new
   3 [ ] OpenCode   new
          ! plugins run
  ... more below

Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x then Enter: never for this folder.`},
		{"short, cursor at the bottom", 200, 9, 2, `  ... more above
   3 [ ] OpenCode   new
          ! plugins run

     Other places this folder's code may be sent
>  4 [ ] OpenCode free models   new
          ! sent to OpenCode

Up/Down move; Space ticks; Enter confirms; a number ticks its row; n or Esc: not now (asked next start); x then Enter: never for this folder.`},
		{"too small for a window", 140, 3, 0, `>  2 [x] Claude Code   new`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSelect(st)
			for i := range c.down {
				s.Press(down, t0.Add(time.Duration(i+1)*time.Second))
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

// TestSelectFrameEnterIgnored (PR #140 re-review, M2): an Enter the select
// ignored says why, and the hint then does not say Enter confirms.
func TestSelectFrameEnterIgnored(t *testing.T) {
	s := NewSelect(NewState([]Row{claude, oc}, Folder{}, false, nil))
	if c := pressAll(s, time.Second, back, enter); c != Nothing {
		t.Fatalf("Enter after a key that did nothing: %d", c)
	}
	want := `   1 [ ] All harnesses

     Harnesses that may run here
>  2 [ ] Claude Code
   3 [ ] OpenCode
          ! plugins run

Enter confirms after a move or a tick: press Up or Down, then Enter. Up/Down move; Space ticks; a number ticks its row; n or Esc: not now (asked next start); x then Enter: never for this folder.`
	if got := strings.Join(s.Frame(220, 40), "\n"); got != want {
		t.Errorf("frame:\n%s\nwant:\n%s", got, want)
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

// agyRow is a harness row All never ticks, as Antigravity's is (T6957).
var agyRow = Row{ID: "antigravity", Label: "Antigravity (Gemini)", Group: Harness, Warning: "terms", NotInAll: true}

// TestSelectNotInAll (T6957, port onto the select): Space on All and the
// digit 1 never tick a row All skips; a re-ask whose only new row is that
// row starts with no cursor, so one Space cannot tick it unseen; and the note
// under the row is drawn with it, the cursor's mark on the row's first line
// only.
func TestSelectNotInAll(t *testing.T) {
	rows := []Row{claude, oc, agyRow}
	for _, keys := range [][]Input{{up, rn(' ')}, {rn('1')}, {up, rn(' '), rn(' '), rn(' ')}, {rn('1'), rn('1'), rn('1')}} {
		s := NewSelect(NewState(rows, Folder{}, false, nil))
		pressAll(s, time.Second, keys...)
		if slices.Contains(s.St.Ticked(), "antigravity") {
			t.Errorf("%v ticked Antigravity: %q", keys, s.St.Ticked())
		}
	}
	answered := Folder{Shown: []string{"claude", "opencode"}, On: []string{"claude"}}
	s := NewSelect(NewState(rows, answered, true, nil))
	if s.At != -1 {
		t.Fatalf("re-asked for Antigravity alone: at %d, want none", s.At)
	}
	if c := pressAll(s, time.Second, rn(' ')); c != Nothing || !slices.Equal(s.St.Ticked(), []string{"claude"}) {
		t.Errorf("Space with no cursor: %d, ticked %q", c, s.St.Ticked())
	}
	s = NewSelect(NewState(rows, Folder{}, false, nil))
	pressAll(s, time.Second, down, down)
	want := `   1 [ ] All harnesses

     Harnesses that may run here
   2 [ ] Claude Code
   3 [ ] OpenCode
          ! plugins run
>  4 [ ] Antigravity (Gemini)
          ! terms
          (All does not tick this one: tick it on its own)

` + SelectKeys
	if got := strings.Join(s.Frame(200, 40), "\n"); got != want {
		t.Errorf("frame:\n%s\nwant:\n%s", got, want)
	}
	// A short screen keeps the note with its row.
	got := s.Frame(200, 8)
	if !slices.Contains(got, "          (All does not tick this one: tick it on its own)") {
		t.Errorf("short frame lost the note:\n%s", strings.Join(got, "\n"))
	}
}
