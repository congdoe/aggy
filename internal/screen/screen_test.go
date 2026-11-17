package screen

import (
	"strings"
	"testing"
)

// view is a screen's lines with trailing blanks cut, and its cursor.
func view(s *Screen) (lines []string, row, col int) {
	p := s.Snapshot()
	for r := range p.Rows {
		lines = append(lines, strings.TrimRight(p.Line(r), " "))
	}
	return lines, p.Row, p.Col
}

// TestFeed (T7016): what the model draws, and where the cursor ends, for the
// sequences Claude Code emits and the edits a CLI may use.
func TestFeed(t *testing.T) {
	for _, c := range []struct {
		name       string
		rows, cols int
		in         string
		lines      []string
		row, col   int
	}{
		{"text and CR LF", 3, 10, "ab\r\ncd", []string{"ab", "cd", ""}, 1, 2},
		{"LF keeps the column", 3, 10, "ab\ncd", []string{"ab", "  cd", ""}, 1, 4},
		{"LF at the bottom scrolls", 2, 10, "a\r\nb\r\nc", []string{"b", "c"}, 1, 1},
		{"wrap at the last column", 2, 3, "abcd", []string{"abc", "d"}, 1, 1},
		{"no wrap until the next character", 2, 3, "abc", []string{"abc", ""}, 0, 2},
		{"autowrap off overwrites", 2, 3, "\x1b[?7labcd", []string{"abd", ""}, 0, 2},
		{"BS", 1, 10, "abc\bX", []string{"abX"}, 0, 3},
		{"TAB", 1, 20, "a\tb", []string{"a       b"}, 0, 9},
		{"CUP and CHA", 3, 10, "\x1b[2J\x1b[2;3Hx\x1b[5Gy", []string{"", "  x y", ""}, 1, 5},
		{"relative moves", 4, 10, "\r\n\r\n\r\n\x1b[2Ax\x1b[3Cy\x1b[2Dz", []string{"", "x  zy", "", ""}, 1, 4},
		{"EL to the end", 1, 10, "abcdef\x1b[3G\x1b[K", []string{"ab"}, 0, 2},
		{"EL to the start", 1, 10, "abcdef\x1b[3G\x1b[1K", []string{"   def"}, 0, 2},
		{"EL whole", 1, 10, "abcdef\x1b[2K", []string{""}, 0, 6},
		{"ED below", 3, 10, "a\r\nb\r\nc\x1b[1;1H\x1b[2J\x1b[Hx\r\ny\r\nz\x1b[2;1H\x1b[J", []string{"x", "", ""}, 1, 0},
		{"ECH", 1, 10, "abcdef\x1b[2G\x1b[2X", []string{"a  def"}, 0, 1},
		{"DCH", 1, 10, "abcdef\x1b[2G\x1b[2P", []string{"adef"}, 0, 1},
		{"ICH", 1, 10, "abcdef\x1b[2G\x1b[2@", []string{"a  bcdef"}, 0, 1},
		{"wide characters", 1, 10, "a漢b", []string{"a漢b"}, 0, 4},
		{"a wide character cut", 1, 10, "a漢b\x1b[3Gx", []string{"a xb"}, 0, 3},
		{"a wide character at the edge wraps", 2, 3, "ab漢", []string{"ab", "漢"}, 1, 2},
		{"zero width", 1, 10, "éx", []string{"e" + "x"}, 0, 2},
		{"save and restore", 2, 10, "ab\x1b7\r\ncd\x1b8x", []string{"abx", "cd"}, 0, 3},
		{"OSC skipped, BEL ended", 1, 10, "a\x1b]0;title\x07b", []string{"ab"}, 0, 2},
		{"OSC skipped, ST ended", 1, 10, "a\x1b]8;;http://x\x1b\\b", []string{"ab"}, 0, 2},
		{"private and query sequences", 1, 10, "a\x1b[>0q\x1b[?u\x1b[c\x1b[>4m\x1b[<u\x1b[?2004hb", []string{"ab"}, 0, 2},
		{"charset and shifts", 1, 10, "a\x1b(B\x0fb", []string{"ab"}, 0, 2},
		{"a control inside a sequence", 2, 10, "a\x1b[\r1Cb", []string{"ab", ""}, 0, 2},
		{"CAN cancels", 1, 10, "a\x1b[3\x18Cb", []string{"aCb"}, 0, 3},
		{"the alternate screen and back", 2, 10, "ab\x1b[?1049hxyz\x1b[?1049lc", []string{"abc", ""}, 0, 3},
		{"IND and NEL", 3, 10, "a\x1bDb\x1bEc", []string{"a", " b", "c"}, 2, 1},
		{"RI", 3, 10, "\r\n\r\na\x1bMb", []string{"", " b", "a"}, 1, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, split := range []bool{false, true} {
				s := New(c.rows, c.cols)
				if split {
					for i := range len(c.in) {
						s.Feed([]byte(c.in[i : i+1]))
					}
				} else {
					s.Feed([]byte(c.in))
				}
				lines, row, col := view(s)
				if strings.Join(lines, "|") != strings.Join(c.lines, "|") || row != c.row || col != c.col {
					t.Errorf("split %v: %q at %d,%d, want %q at %d,%d", split, lines, row, col, c.lines, c.row, c.col)
				}
			}
		})
	}
}

// TestFaint (T7016): SGR 2 marks cells faint, SGR 22 and 0 end it, and
// colour parameters (whose values may be 2 or 22) do not touch it.
func TestFaint(t *testing.T) {
	for _, c := range []struct {
		name  string
		in    string
		faint string // per column, f for faint
	}{
		{"on and off", "a\x1b[2mbc\x1b[22md", " ff "},
		{"reset", "\x1b[2ma\x1b[0mb\x1b[2mc\x1b[md", "f f "},
		{"bold off ends it", "\x1b[2ma\x1b[1;22mb", "f "},
		{"truecolour with a 2", "\x1b[38;2;2;22;0ma", " "},
		{"truecolour after faint", "\x1b[2;38;2;0;0;0ma", "f"},
		{"256 colours with a 2", "\x1b[38;5;2ma\x1b[2mb", " f"},
		{"colon form", "\x1b[38:2::2:2:2;2ma", "f"},
		{"modifyOtherKeys is no SGR", "\x1b[>2ma", " "},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New(1, 10)
			s.Feed([]byte(c.in))
			p := s.Snapshot()
			var got strings.Builder
			for col := range len(c.faint) {
				if p.Cell(0, col).Faint {
					got.WriteByte('f')
				} else {
					got.WriteByte(' ')
				}
			}
			if got.String() != c.faint {
				t.Errorf("faint %q, want %q", got.String(), c.faint)
			}
		})
	}
}

// TestUnsure (T7016): what the model does not follow makes it Unsure, and
// only a full clear (or a reset) ends that; what it follows does not.
func TestUnsure(t *testing.T) {
	for _, c := range []struct {
		name   string
		in     string
		unsure bool
	}{
		{"plain text", "abc\r\n", false},
		{"Claude's start", "\x1b7\x1b[r\x1b8\x1b[?25h\x1b[?25l\x1b[?2004h\x1b[>0q\x1b[?u\x1b]7501;?\x07\x1b[c\x1b[>4m\x1b[<u", false},
		{"a scroll region", "\x1b[2;5r", true},
		{"a whole-screen scroll region", "\x1b7\x1b[1;10r\x1b8", false},
		{"an absolute row, then text", "\x1b[3;1Hx", true},
		{"an absolute row, then a full clear", "\x1b[3;1Hx\x1b[2J", false},
		{"home, then ED 0", "a\x1b[Hx\x1b[H\x1b[J", false},
		{"a cursor report", "\x1b[6n", true},
		{"origin mode", "\x1b[?6h", true},
		{"insert mode", "\x1b[4h", true},
		{"newline mode", "\x1b[20h", true},
		{"an unknown private mode", "\x1b[?9999h", true},
		{"a harmless private mode", "\x1b[?1004h\x1b[?2026h\x1b[?1049h\x1b[?1049l", false},
		{"DEC line drawing", "\x1b(0q", true},
		{"SO into an unset G1", "\x1b)0\x0eq", true},
		{"SS2", "\x1bNa", true},
		{"DECALN", "\x1b#8", true},
		{"a sixel", "\x1bPq#0;2;0;0;0~\x1b\\", true},
		{"a capability query", "\x1bP+q544e\x1b\\", false},
		{"APC", "\x1b_Ga=T\x1b\\", true},
		{"invalid UTF-8", "a\xffb", true},
		{"a cut UTF-8 sequence", "\xe2\x94x", true},
		{"a C1 control", "\xc2\x9b", true},
		{"REP", "a\x1b[3b", true},
		{"an unknown final", "\x1b[3y", true},
		{"DECSTR", "\x1b[!p", true},
		{"a cursor style", "\x1b[2 q", false},
		{"up past the start", "\r\n\x1b[2A", true},
		{"down past what was drawn", "\r\n\r\n\x1b[2A\x1b[3B", true},
		{"down within what was drawn", "\r\n\r\n\x1b[2A\x1b[2B", false},
		{"RI on the top row", "\x1bM", true},
		{"insert lines", "\r\n\x1b[L", true},
		{"BS from the wrap position", "\x1b[10Gx\b", true},
		{"too many parameters", "\x1b[1;2;3;4;5;6;7;8;9;10;11;12;13;14;15;16;17m", true},
		{"a reset", "\x1b[3;1Hx\x1bc", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New(5, 10)
			s.Feed([]byte(c.in))
			if got := s.Snapshot().Unsure; got != c.unsure {
				t.Errorf("Unsure %v, want %v", got, c.unsure)
			}
		})
	}
}

// TestUnsureSticks (T7016; #149 review): content doubt lasts through
// ordinary drawing and ends at a full clear; a mode lasts until it is turned
// off, whatever is cleared; an unknown one until a reset; a resize makes
// doubt, a size out of range keeps it.
func TestUnsureSticks(t *testing.T) {
	type step struct {
		in     string
		unsure bool
	}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"content doubt ends at a full clear", []step{{"\r\n\x1b[2A", true}, {"abc\x1b[K", true}, {"\x1b[2J", false}}},
		{"a scroll region lasts until reset", []step{{"\x1b[2;3r", true}, {"\x1b[2J", true}, {"\x1b[r", false}}},
		{"insert mode lasts until off", []step{{"\x1b[4h", true}, {"\x1b[2J", true}, {"\x1b[4l", false}}},
		{"newline mode lasts until off", []step{{"\x1b[20h", true}, {"\x1b[H\x1b[J", true}, {"\x1b[20l", false}}},
		{"origin mode lasts until off", []step{{"\x1b[?6h", true}, {"\x1b[2J", true}, {"\x1b[?6l", false}}},
		{"an unknown mode lasts until RIS", []step{{"\x1b[?9999h", true}, {"\x1b[2J", true}, {"\x1b[?9999l", true}, {"\x1bc", false}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New(5, 10)
			for i, st := range c.steps {
				s.Feed([]byte(st.in))
				if got := s.Snapshot().Unsure; got != st.unsure {
					t.Fatalf("after step %d %q: Unsure %v, want %v", i, st.in, got, st.unsure)
				}
			}
		})
	}
	s := New(5, 10)
	s.Resize(5, 10)
	if s.Snapshot().Unsure {
		t.Fatal("the same size made it Unsure")
	}
	s.Resize(6, 10)
	if !s.Snapshot().Unsure {
		t.Fatal("a resize did not make it Unsure")
	}
	s.Feed([]byte("\x1b[2J"))
	if s.Snapshot().Unsure {
		t.Fatal("a clear after a resize did not end it")
	}
	s.Resize(MaxRows+1, 10)
	s.Feed([]byte("\x1b[2J"))
	if !s.Snapshot().Unsure {
		t.Fatal("a size out of range is not Unsure")
	}
	s.Resize(5, 10)
	s.Feed([]byte("\x1b[2J"))
	if s.Snapshot().Unsure {
		t.Fatal("a size in range and a clear did not end it")
	}
}

// TestGapNeedsReset (#149 review, M2): bytes never fed may have hidden the
// cursor or turned faint off, so after a gap a full clear does not restore
// trust; only a reset does.
func TestGapNeedsReset(t *testing.T) {
	s := New(5, 10)
	s.SetUnsure()
	s.Feed([]byte("\x1b[H\x1b[2Jabc"))
	if !s.Snapshot().Unsure {
		t.Fatal("a full clear after a gap ended Unsure")
	}
	s.Feed([]byte("\x1bc"))
	if s.Snapshot().Unsure {
		t.Fatal("a reset did not end it")
	}
}

// TestAltClearKeepsMain (#149 review, M1): a clear on the alternate screen
// says nothing of the main screen's rows: back on it, with the cursor's row
// known from the alternate screen, the main screen stays Unsure; nor does it
// end the main screen's doubt.
func TestAltClearKeepsMain(t *testing.T) {
	for _, mode := range []string{"47", "1047", "1049"} {
		s := New(10, 20)
		s.Feed([]byte("ab\r\ncd\r\n"))
		s.Feed([]byte("\x1b[?" + mode + "h\x1b[H\x1b[2J\x1b[?" + mode + "l\x1b[2;3H"))
		if !s.Snapshot().Unsure {
			t.Errorf("?%s: the main screen's rows taken as known", mode)
		}
	}
	s := New(10, 20)
	s.Feed([]byte("\r\n\x1b[3A\x1b[?1049h\x1b[2J\x1b[?1049l"))
	if !s.Snapshot().Unsure {
		t.Error("a clear on the alternate screen ended the main screen's doubt")
	}
}

// TestAnchorOnBlank (#149 review, L2): a full clear, then home (Ink's order),
// makes the rows real, as home then a clear does; a redraw from home that
// erases every row with EL 2 before drawing counts as a full clear (Claude
// Code 2.1.326's, in the recording's exit), and one that draws before the
// last row, leaves one, or scrolls does not.
func TestAnchorOnBlank(t *testing.T) {
	for _, c := range []struct {
		name   string
		in     string
		unsure bool
	}{
		{"clear then home", "x\x1b[2J\x1b[3J\x1b[Hab", false},
		{"home then clear", "x\x1b[H\x1b[2J\x1b[3Jab", false},
		{"clear, text, then home", "x\x1b[2Jy\x1b[Hab", true},
		{"every row erased from home", "x\r\n\x1b[H\x1b[2K\x1b[1B\x1b[2K\x1b[1B\x1b[2K\x1b[Hab", false},
		{"a row left", "x\r\n\x1b[H\x1b[2K\x1b[1B\x1b[2K\x1b[Hab", true},
		{"text between", "x\r\n\x1b[H\x1b[2Kq\x1b[1B\x1b[2K\x1b[1B\x1b[2K\x1b[Hab", true},
		{"a scroll between", "x\r\n\x1b[H\x1b[2K\x1b[1B\x1b[2K\n\n\n\x1b[2K\x1b[Hab", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New(3, 10)
			s.Feed([]byte(c.in))
			if got := s.Snapshot().Unsure; got != c.unsure {
				t.Errorf("Unsure %v, want %v", got, c.unsure)
			}
		})
	}
}

// TestFeedBudget (#149 review, L4): a flood of whole-screen operations stops
// being followed past the budget, and the screen is lost until a reset.
func TestFeedBudget(t *testing.T) {
	for _, flood := range []string{"\x1b[2J", "\x1bc", "\x1b[299S", "\x1b[299L", "\x1b[299M", "\x1b[299T"} {
		s := New(MaxRows, MaxCols)
		in := []byte(strings.Repeat(flood, 4096))
		s.Feed(in)
		if s.work > s.feedBudget(len(in))+4*MaxRows*MaxCols {
			t.Errorf("%q: %d cells touched", flood, s.work)
		}
		if !s.lost || !s.Snapshot().Unsure {
			t.Errorf("%q: past the budget and not lost", flood)
		}
	}
}

// TestBounded (T7016): hostile sizes and parameters stay in bounds.
func TestBounded(t *testing.T) {
	for _, in := range []string{
		"\x1b[99999999999999;99999999999999H", "\x1b[99999999999A", "\x1b[99999999999B", "\x1b[99999999999@",
		"\x1b[99999999999P", "\x1b[99999999999X", "\x1b[99999999999L", "\x1b[99999999999S", "\x1b[99999999999T",
		"\x1b[99999999999I", "\x1b[99999999999Z", "\x1b[99999999999G", strings.Repeat("\x1b]", 1000) + strings.Repeat("x", 10000),
	} {
		for _, size := range [][2]int{{1, 1}, {3, 4}, {0, 0}, {-1, 5}, {MaxRows + 5, MaxCols + 5}} {
			s := New(size[0], size[1])
			s.Feed([]byte(in))
			checkBounds(t, s)
		}
	}
}

func checkBounds(t *testing.T, s *Screen) {
	t.Helper()
	p := s.Snapshot()
	if p.Rows < 1 || p.Cols < 1 || p.Rows > MaxRows || p.Cols > MaxCols {
		t.Fatalf("size %dx%d", p.Rows, p.Cols)
	}
	if p.Row < 0 || p.Row >= p.Rows || p.Col < 0 || p.Col >= p.Cols {
		t.Fatalf("cursor %d,%d outside %dx%d", p.Row, p.Col, p.Rows, p.Cols)
	}
	for _, g := range s.grids {
		if len(g) != p.Rows {
			t.Fatalf("grid of %d rows, screen %d", len(g), p.Rows)
		}
		for _, r := range g {
			if len(r) != p.Cols {
				t.Fatalf("row of %d cells, screen %d", len(r), p.Cols)
			}
		}
	}
	for _, sv := range s.saved {
		if sv.row < 0 || sv.row >= p.Rows || sv.col < 0 || sv.col >= p.Cols {
			t.Fatalf("saved cursor %d,%d outside %dx%d", sv.row, sv.col, p.Rows, p.Cols)
		}
	}
}

// FuzzFeed (T7016): any output, in any split, never panics, and the cursor
// and the grids stay in bounds, through resizes too.
func FuzzFeed(f *testing.F) {
	for _, seed := range []string{
		"\x1b7\x1b[r\x1b8\x1b[?25h", "a漢\x1b[2D\x1b[P", "\x1b[?1049h\x1b[2J\x1b[?1049l", "\x1b]0;x\x07\x1bP+q\x1b\\",
		"\x1b[38;2;1;2;3;2m\xe2\x9d\xaf\xc2\xa0\x1b[2mTry\x1b[22m", "\x1b[3;5r\x1bM\x1bD\x1b[L\x1b[M\x1b[S\x1b[T",
	} {
		f.Add([]byte(seed), uint8(5), uint8(10), uint8(3))
	}
	f.Fuzz(func(t *testing.T, b []byte, rows, cols, split uint8) {
		s := New(int(rows%40), int(cols%90))
		step := int(split%16) + 1
		for i := 0; i < len(b); i += step {
			s.Feed(b[i:min(i+step, len(b))])
			if i%97 == 0 {
				s.Resize(int(rows%40)+i%3, int(cols%90)+i%5)
			}
		}
		checkBounds(t, s)
		_ = s.Snapshot().Line(0)
	})
}
