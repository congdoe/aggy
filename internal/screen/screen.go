package screen

import "strings"

// MaxRows and MaxCols bound the screen modelled: a larger size is Unsure.
const (
	MaxRows = 300
	MaxCols = 600
)

// maxParams bounds a CSI sequence's parameters, and maxParam each one's value;
// a sequence past either is not acted on.
const (
	maxParams = 16
	maxParam  = 9999
)

// Cell is one column of a row.
type Cell struct {
	// R is the character, 0 for a blank never drawn or erased.
	R rune
	// Wide: R takes this column and the next; Cont: this column is the second
	// half of the wide character before it.
	Wide, Cont bool
	// Faint: drawn with SGR 2 on.
	Faint bool
}

// Blank reports whether c shows nothing: never drawn, erased, or a space.
func (c Cell) Blank() bool { return !c.Cont && (c.R == 0 || c.R == ' ') }

// cursor is the cursor and what DECSC saves with it.
type cursor struct {
	row, col int
	// wrapNext: a character was drawn in the last column, and the next one
	// wraps first.
	wrapNext bool
	faint    bool
	// rowKnown: row is the screen's real row, not one relative to where the
	// CLI started (see the package doc).
	rowKnown bool
	g        [2]byte
	shift    int
}

// parser states.
const (
	stGround = iota
	stEsc
	stEscInter // ESC and an intermediate byte, waiting for the final one
	stCSI
	stOSC
	stOSCEsc
	stStr // DCS, APC, PM, or SOS: skipped
	stStrEsc
)

// Screen is the model. Its methods are not safe for concurrent use.
type Screen struct {
	rows, cols int
	grids      [2][][]Cell
	alt        bool
	cur        cursor
	saved      [2]cursor
	hasSaved   [2]bool
	visible    bool
	autowrap   bool
	// unsure, per buffer (main, alternate): its content may differ from the
	// real screen's, until a full clear of that buffer. lost: a state aggy does
	// not model may be on (a mode, a chunk of output never fed), until a reset
	// (RIS): a clear blanks cells, not modes. irm, lnm, origin, and region:
	// insert mode, newline mode, origin mode, and a scroll region that is not
	// the whole screen are on, until turned off. bad: a size out of range,
	// until a size in range.
	unsure                   [2]bool
	lost, bad                bool
	irm, lnm, origin, region bool
	// anchored, per buffer: its rows are the real ones (a full clear came with
	// the row known, or the row became known on a buffer blank since a full
	// clear). blank, per buffer: nothing drawn or erased since a full clear.
	// maxRow is the lowest row the cursor reached since the start or the last
	// full clear, while rows are relative.
	anchored, blank [2]bool
	maxRow          int
	// wipe follows a redraw from home that erases every row with EL 2 before
	// any other write (Claude's, T7016 review): wipeRows are the rows erased,
	// wipeN how many; once all are, it counts as a full clear.
	wipeOn   bool
	wipeRows []bool
	wipeN    int
	// work counts the cells a Feed call has touched (feedBudget).
	work int

	st       int
	params   [maxParams]int
	sub      [maxParams]bool
	np       int
	inParam  bool
	tooMany  bool
	priv     byte
	inter    []byte
	interBuf [4]byte
	badSeq   bool
	escInter byte
	strHead  []byte
	strBuf   [2]byte

	utf     [4]byte
	utfN    int
	utfNeed int
}

// New returns a blank screen of rows by cols.
func New(rows, cols int) *Screen {
	s := &Screen{visible: true, autowrap: true}
	s.cur.g = [2]byte{'B', 'B'}
	s.alloc(rows, cols)
	return s
}

func inRange(rows, cols int) bool {
	return rows >= 1 && cols >= 1 && rows <= MaxRows && cols <= MaxCols
}

// alloc makes both grids rows by cols, keeping what fits of the old ones.
func (s *Screen) alloc(rows, cols int) {
	if !inRange(rows, cols) {
		s.bad = true
		rows, cols = max(1, min(rows, MaxRows)), max(1, min(cols, MaxCols))
	} else {
		s.bad = false
	}
	for i := range s.grids {
		g := make([][]Cell, rows)
		for r := range g {
			g[r] = make([]Cell, cols)
			if r < len(s.grids[i]) {
				copy(g[r], s.grids[i][r])
			}
		}
		s.grids[i] = g
	}
	s.rows, s.cols = rows, cols
	s.wipeRows, s.wipeOn = make([]bool, rows), false
	for r := range s.rows {
		s.fixWide(r)
	}
	clampCur := func(c *cursor) {
		c.row, c.col = min(c.row, rows-1), min(c.col, cols-1)
		c.wrapNext = false
	}
	clampCur(&s.cur)
	clampCur(&s.saved[0])
	clampCur(&s.saved[1])
	s.maxRow = min(s.maxRow, rows-1)
}

// Resize sets the screen's size, as the pseudo-terminal's was set. A change
// makes the screen Unsure: terminals differ in how they reflow on a resize.
func (s *Screen) Resize(rows, cols int) {
	if rows == s.rows && cols == s.cols && inRange(rows, cols) {
		return
	}
	s.alloc(rows, cols)
	s.unsure = [2]bool{true, true}
	s.anchored, s.blank = [2]bool{}, [2]bool{}
}

// SetUnsure marks the screen Unsure until a reset (RIS): its owner lost
// bytes, which may have held a mode or the cursor's visibility, so a full
// clear does not end it (T7016 review).
func (s *Screen) SetUnsure() { s.lost = true }

// feedBudget bounds the cells one Feed call may touch: per byte, and a few
// whole screens. Output past it (a flood of clears or scrolls) is not
// followed: the screen is lost until a reset (T7016 review).
//
// TODO(T7016): at the largest sizes (300 by 600) legitimate output that erases
// long lines often can come near the budget and lose the copy (#149 review,
// N3); a per-session budget would be kinder there.
func (s *Screen) feedBudget(n int) int { return 256*n + 4*s.rows*s.cols + 4096 }

// Feed takes the CLI's output, in any split across calls.
func (s *Screen) Feed(b []byte) {
	s.work = 0
	budget := s.feedBudget(len(b))
	for _, c := range b {
		if s.work > budget {
			s.lost = true
			s.st, s.utfN, s.utfNeed = stGround, 0, 0
			return
		}
		s.byte(c)
		s.work++
	}
}

// doubt marks the content of the buffer in use as possibly not the real one's.
func (s *Screen) doubt() { s.unsure[s.buf()] = true }

func (s *Screen) grid() [][]Cell {
	if s.alt {
		return s.grids[1]
	}
	return s.grids[0]
}

func (s *Screen) buf() int {
	if s.alt {
		return 1
	}
	return 0
}

// byte is the parser: one byte of output.
func (s *Screen) byte(c byte) {
	// A C0 control acts inside escape and control sequences too, as in xterm,
	// except in the strings, which it ends or belongs to.
	switch s.st {
	case stOSC, stOSCEsc, stStr, stStrEsc:
		s.strByte(c)
		return
	}
	if c < 0x20 || c == 0x7f {
		if s.utfNeed > 0 {
			s.badUTF()
		}
		s.control(c)
		return
	}
	switch s.st {
	case stGround:
		s.text(c)
	case stEsc:
		s.escByte(c)
	case stEscInter:
		s.escInterByte(c)
	case stCSI:
		s.csiByte(c)
	}
}

// control acts on a C0 control byte or DEL.
func (s *Screen) control(c byte) {
	switch c {
	case 0x1b:
		s.st = stEsc
		return
	case 0x18, 0x1a: // CAN, SUB: the sequence is cancelled
		s.st = stGround
		return
	case 0x08:
		if s.cur.wrapNext {
			s.doubt() // terminals differ on BS from the wrap position
		}
		s.cur.wrapNext = false
		if s.cur.col > 0 {
			s.cur.col--
		}
	case 0x09:
		s.cur.wrapNext = false
		s.cur.col = min((s.cur.col/8+1)*8, s.cols-1)
	case 0x0a, 0x0b, 0x0c:
		s.lineFeed()
	case 0x0d:
		s.cur.col, s.cur.wrapNext = 0, false
	case 0x0e:
		s.cur.shift = 1
	case 0x0f:
		s.cur.shift = 0
	}
}

// strByte follows an OSC or another string sequence to its end: BEL (OSC
// only) or ST. An ESC with anything else ends it and starts that sequence.
func (s *Screen) strByte(c byte) {
	switch s.st {
	case stOSC, stStr:
		switch c {
		case 0x07:
			if s.st == stOSC {
				s.st = stGround
				return
			}
			s.strStep(c)
		case 0x1b:
			s.st++ // stOSCEsc or stStrEsc
		case 0x18, 0x1a:
			s.st = stGround
		default:
			s.strStep(c)
		}
	case stOSCEsc, stStrEsc:
		if c == '\\' {
			s.st = stGround
			return
		}
		s.st = stEsc
		s.byte(c)
	}
}

// strStep takes a byte of a string sequence: a DCS other than a query is
// Unsure (sixel and passthrough draw).
func (s *Screen) strStep(c byte) {
	if s.st != stStr || s.strHead == nil || len(s.strHead) >= 2 {
		return
	}
	s.strHead = append(s.strHead, c)
	if len(s.strHead) == 2 {
		h := string(s.strHead)
		if h != "+q" && h != "$q" {
			s.lost = true
		}
	}
}

func (s *Screen) escByte(c byte) {
	s.st = stGround
	switch c {
	case '[':
		s.st = stCSI
		s.np, s.inParam, s.tooMany, s.priv, s.badSeq = 0, false, false, 0, false
		s.inter = s.interBuf[:0]
		s.params = [maxParams]int{}
		s.sub = [maxParams]bool{}
	case ']':
		s.st = stOSC
	case 'P':
		s.st, s.strHead = stStr, s.strBuf[:0]
	case 'X', '^', '_':
		s.st, s.strHead = stStr, nil
		s.lost = true // APC draws images in some terminals; PM and SOS are unknown
	case '7':
		s.saveCursor()
	case '8':
		s.restoreCursor()
	case 'c':
		s.reset()
	case 'D':
		s.lineFeed()
	case 'E':
		s.cur.col = 0
		s.lineFeed()
	case 'M':
		s.reverseIndex()
	case '=', '>', '\\':
	case '(', ')', '*', '+', '-', '.', '/', '#', ' ', '%':
		s.st, s.escInter = stEscInter, c
	default:
		s.lost = true // HTS, SS2, SS3, LS2, LS3, and anything else
	}
}

func (s *Screen) escInterByte(c byte) {
	s.st = stGround
	switch s.escInter {
	case '(':
		s.cur.g[0] = c
	case ')':
		s.cur.g[1] = c
	case '*', '+', '-', '.', '/':
		// G2 and G3 are reached only through SS2, SS3, LS2, and LS3, which are
		// Unsure; G1 through 96-character sets is not ASCII.
		if s.escInter == '-' {
			s.cur.g[1] = 0
		}
	case ' ':
	case '%':
		if c != 'G' {
			s.lost = true // out of UTF-8
		}
	default: // '#': line sizes and DECALN
		s.lost = true
	}
}

func (s *Screen) csiByte(c byte) {
	switch {
	case c >= '0' && c <= '9':
		if len(s.inter) > 0 {
			s.badSeq = true
		}
		if s.np >= maxParams {
			s.tooMany = true
			return
		}
		if !s.inParam {
			s.inParam = true
		}
		if !s.sub[s.np] {
			s.params[s.np] = min(s.params[s.np]*10+int(c-'0'), maxParam)
		}
	case c == ';':
		if len(s.inter) > 0 {
			s.badSeq = true
		}
		if s.np < maxParams {
			s.np++
		} else {
			s.tooMany = true
		}
		s.inParam = false
	case c == ':':
		if s.np < maxParams {
			s.sub[s.np] = true
		}
	case c >= '<' && c <= '?':
		if s.np == 0 && !s.inParam && s.priv == 0 && !s.sub[0] {
			s.priv = c
		} else {
			s.badSeq = true
		}
	case c >= 0x20 && c <= 0x2f:
		if len(s.inter) < cap(s.inter) {
			s.inter = append(s.inter, c)
		} else {
			s.badSeq = true
		}
	case c >= 0x40 && c <= 0x7e:
		s.st = stGround
		n := s.np
		if s.inParam || s.np > 0 {
			n = min(s.np+1, maxParams)
		}
		if s.badSeq || s.tooMany {
			s.lost = true
			return
		}
		s.dispatch(c, n)
	default:
		// 0x80 and up inside a sequence: xterm ignores them.
	}
}

// param is parameter i, or def when it is missing or zero.
func (s *Screen) param(i, n, def int) int {
	if i >= n || s.params[i] == 0 {
		return def
	}
	return s.params[i]
}

func (s *Screen) dispatch(f byte, n int) {
	if len(s.inter) > 0 {
		switch string(s.inter) + string(f) {
		case " q", "$p", " t", " u":
			return // cursor style, a mode query, bell volumes
		}
		s.lost = true
		return
	}
	switch s.priv {
	case '>', '<', '=':
		return // key, title, and pointer modes, and queries: no cells move
	case '?':
		s.privDispatch(f, n)
		return
	}
	p1 := s.param(0, n, 1)
	switch f {
	case 'A':
		s.moveUp(p1)
	case 'B', 'e':
		s.moveDown(p1)
	case 'C', 'a':
		s.moveRight(p1)
	case 'D':
		if s.cur.wrapNext {
			s.doubt()
		}
		s.cur.wrapNext = false
		s.cur.col = max(s.cur.col-p1, 0)
	case 'E':
		s.moveDown(p1)
		s.cur.col = 0
	case 'F':
		s.moveUp(p1)
		s.cur.col = 0
	case 'G', '`':
		s.cur.wrapNext = false
		s.cur.col = min(p1, s.cols) - 1
	case 'H', 'f':
		s.setRow(s.param(0, n, 1) - 1)
		s.cur.col = min(s.param(1, n, 1), s.cols) - 1
		if s.cur.row == 0 {
			s.startWipe()
		}
	case 'd':
		s.setRow(p1 - 1)
	case 'I':
		s.cur.wrapNext = false
		for range min(p1, s.cols) {
			s.cur.col = min((s.cur.col/8+1)*8, s.cols-1)
		}
	case 'Z':
		s.cur.wrapNext = false
		for range min(p1, s.cols) {
			if s.cur.col > 0 {
				s.cur.col = (s.cur.col - 1) / 8 * 8
			}
		}
	case 'J':
		s.eraseDisplay(s.param(0, n, 0))
	case 'K':
		s.eraseLine(s.param(0, n, 0))
	case 'X':
		s.edit()
		s.clearCells(s.cur.row, s.cur.col, min(s.cur.col+p1, s.cols))
	case 'P':
		s.deleteChars(p1)
	case '@':
		s.insertChars(p1)
	case 'L', 'M', 'S':
		s.lines(f, p1)
	case 'T':
		if n > 1 {
			s.lost = true // the mouse highlight form
			return
		}
		s.lines(f, p1)
	case 'm':
		s.sgr(n)
	case 'r':
		top, bottom := s.param(0, n, 1), s.param(1, n, s.rows)
		s.setMode(&s.region, top != 1 || bottom < s.rows) // a scroll region aggy does not model
		s.cur.wrapNext = false
		s.setRow(0)
		s.cur.col = 0
	case 's':
		if n > 0 {
			s.lost = true // left and right margins
			return
		}
		s.saveCursor()
	case 'u':
		s.restoreCursor()
	case 'h', 'l':
		for i := range n {
			switch s.params[i] {
			case 4:
				s.setMode(&s.irm, f == 'h') // insert mode
			case 20:
				s.setMode(&s.lnm, f == 'h') // newline mode
			}
		}
	case 'c', 'q', 't', 'x', 'i':
		// Resets, device attributes, LEDs, window reports, printer: no cells.
	case 'n':
		if s.param(0, n, 0) == 6 && !s.anchored[s.buf()] {
			s.doubt() // the CLI learns the real row (see the package doc)
		}
	default:
		s.lost = true // tab clears, REP, and anything else
	}
}

// harmlessModes are DEC private modes that change no cell and no cursor
// place: keys, the mouse, focus, paste, synchronized output, blinking, and
// the like.
var harmlessModes = map[int]bool{
	1: true, 2: true, 4: true, 5: true, 8: true, 9: true, 10: true, 12: true, 13: true, 14: true, 18: true, 19: true,
	30: true, 35: true, 40: true, 41: true, 42: true, 44: true, 45: true, 66: true, 67: true,
	1000: true, 1001: true, 1002: true, 1003: true, 1004: true, 1005: true, 1006: true, 1007: true, 1010: true,
	1011: true, 1015: true, 1016: true, 1034: true, 1035: true, 1036: true, 1037: true, 1039: true, 1040: true,
	1041: true, 1042: true, 1043: true, 1044: true, 1050: true, 1051: true, 1052: true, 1053: true, 1060: true,
	1061: true, 2004: true, 2026: true, 2027: true, 2031: true, 2048: true,
}

func (s *Screen) privDispatch(f byte, n int) {
	switch f {
	case 'h', 'l':
		on := f == 'h'
		for i := range n {
			s.privMode(s.params[i], on)
		}
	case 'J':
		s.eraseDisplay(s.param(0, n, 0))
	case 'K':
		s.eraseLine(s.param(0, n, 0))
	case 'n':
		if s.param(0, n, 0) == 6 && !s.anchored[s.buf()] {
			s.doubt()
		}
	case 'u', 'c', 's', 'm', 'q':
		// Kitty's key query, device attributes, XTSAVE, key-modifier queries.
	default:
		s.lost = true // XTRESTORE and anything else
	}
}

func (s *Screen) privMode(m int, on bool) {
	switch m {
	case 25:
		s.visible = on
	case 7:
		s.autowrap = on
		if !on {
			s.cur.wrapNext = false
		}
	case 47, 1047:
		s.setAlt(on, false, m == 1047 && !on)
	case 1049:
		if on {
			if !s.alt {
				s.saveCursor()
			}
			s.setAlt(true, true, false)
		} else if s.alt {
			s.setAlt(false, false, false)
			s.restoreCursor()
		}
	case 1048:
		if on {
			s.saveCursor()
		} else {
			s.restoreCursor()
		}
	case 6:
		s.setMode(&s.origin, on) // homes the cursor, in either direction
		s.setRow(0)
		s.cur.col = 0
	default:
		if !harmlessModes[m] {
			s.lost = true // 132 columns, side margins, and the rest
		}
	}
}

// setAlt moves to the alternate screen or back. clearOn clears it on the way
// in; clearOff clears it on the way out.
func (s *Screen) setAlt(on, clearOn, clearOff bool) {
	s.wipeOn = false
	if on == s.alt {
		if on && clearOn {
			s.clearGrid(s.grids[1])
		}
		return
	}
	if !on && clearOff {
		s.clearGrid(s.grids[1])
	}
	s.alt = on
	if on && clearOn {
		s.clearGrid(s.grids[1])
	}
}

func (s *Screen) clearGrid(g [][]Cell) {
	s.work += s.rows * s.cols
	for _, r := range g {
		clear(r)
	}
}

func (s *Screen) sgr(n int) {
	if n == 0 {
		s.cur.faint = false
		return
	}
	for i := 0; i < n; i++ {
		switch p := s.params[i]; p {
		case 0, 22:
			s.cur.faint = false
		case 2:
			s.cur.faint = true
		case 38, 48, 58:
			if s.sub[i] || i+1 >= n {
				continue
			}
			switch s.params[i+1] {
			case 5:
				i += 2
			case 2:
				i += 4
			default:
				return // malformed: the rest is not read
			}
		}
	}
}

// saveCursor is DECSC, for the screen in use.
func (s *Screen) saveCursor() {
	b := s.buf()
	s.saved[b], s.hasSaved[b] = s.cur, true
}

// restoreCursor is DECRC. With nothing saved the cursor goes home.
func (s *Screen) restoreCursor() {
	b := s.buf()
	if !s.hasSaved[b] {
		s.cur = cursor{g: [2]byte{'B', 'B'}}
		s.setRow(0)
		return
	}
	s.cur = s.saved[b]
	if s.anchored[b] && !s.cur.rowKnown {
		s.doubt() // a place saved before the screen's rows were known
	}
}

// reset is RIS: a blank screen, all modes as at start, rows known.
func (s *Screen) reset() {
	s.clearGrid(s.grids[0])
	s.clearGrid(s.grids[1])
	s.alt, s.visible, s.autowrap = false, true, true
	s.cur = cursor{g: [2]byte{'B', 'B'}, rowKnown: true}
	s.hasSaved = [2]bool{}
	s.anchored, s.blank, s.unsure, s.maxRow = [2]bool{true, true}, [2]bool{true, true}, [2]bool{}, 0
	s.lost, s.irm, s.lnm, s.origin, s.region, s.wipeOn = false, false, false, false, false, false
}

// setRow puts the cursor on an absolute row. On a buffer blank since a full
// clear, nothing drawn can be off, so its rows become real (T7016 review: Ink
// clears, then homes).
func (s *Screen) setRow(r int) {
	s.cur.wrapNext = false
	s.cur.row = max(0, min(r, s.rows-1))
	s.cur.rowKnown = true
	if b := s.buf(); s.blank[b] {
		s.anchored[b] = true
	}
}

// relative reports whether rows are known only relative to the CLI's start.
func (s *Screen) relative() bool { return !s.cur.rowKnown && !s.anchored[s.buf()] }

func (s *Screen) moveUp(n int) {
	s.cur.wrapNext = false
	if s.cur.row-n < 0 && s.relative() {
		s.doubt() // the real screen may have room above
	}
	s.cur.row = max(s.cur.row-n, 0)
}

func (s *Screen) moveDown(n int) {
	s.cur.wrapNext = false
	t := min(s.cur.row+n, s.rows-1)
	if s.relative() && s.cur.row+n > s.maxRow {
		s.doubt() // the real screen may end sooner
	}
	s.cur.row = t
}

func (s *Screen) moveRight(n int) {
	s.cur.wrapNext = false
	s.cur.col = min(s.cur.col+n, s.cols-1)
}

func (s *Screen) lineFeed() {
	s.cur.wrapNext = false
	if s.cur.row == s.rows-1 {
		s.scrollUp(1)
	} else {
		s.cur.row++
	}
	if s.relative() {
		s.maxRow = max(s.maxRow, s.cur.row)
	}
}

func (s *Screen) reverseIndex() {
	s.cur.wrapNext = false
	if s.cur.row > 0 {
		s.cur.row--
		return
	}
	if s.relative() {
		s.doubt() // the real cursor may not be on the top row
	}
	s.edit()
	s.scrollDown(1)
}

func (s *Screen) scrollUp(n int) {
	g := s.grid()
	n = min(n, s.rows)
	s.wipeOn = false
	s.work += n * (s.rows + s.cols)
	if s.region {
		s.doubt() // the real terminal scrolls the region only
	}
	for range n {
		first := g[0]
		copy(g, g[1:])
		clear(first)
		g[len(g)-1] = first
	}
}

func (s *Screen) scrollDown(n int) {
	g := s.grid()
	n = min(n, s.rows)
	s.wipeOn = false
	s.work += n * (s.rows + s.cols)
	if s.region {
		s.doubt() // the real terminal scrolls the region only
	}
	for range n {
		last := g[len(g)-1]
		copy(g[1:], g[:len(g)-1])
		clear(last)
		g[0] = last
	}
}

// lines is IL, DL, SU, and SD, on the whole screen (a scroll region is
// Unsure already).
func (s *Screen) lines(f byte, n int) {
	if s.relative() {
		s.doubt() // the real screen's bottom is not known
	}
	s.edit()
	s.cur.wrapNext = false
	g := s.grid()
	n = min(n, s.rows)
	s.work += n * (s.rows + s.cols)
	switch f {
	case 'S':
		s.scrollUp(n)
	case 'T':
		s.scrollDown(n)
	case 'L':
		sub := g[s.cur.row:]
		for range min(n, len(sub)) {
			last := sub[len(sub)-1]
			copy(sub[1:], sub[:len(sub)-1])
			clear(last)
			sub[0] = last
		}
		s.cur.col = 0
	case 'M':
		sub := g[s.cur.row:]
		for range min(n, len(sub)) {
			first := sub[0]
			copy(sub, sub[1:])
			clear(first)
			sub[len(sub)-1] = first
		}
		s.cur.col = 0
	}
}

// edit notes a change to cells: with the cursor's row real and the content's
// rows relative, the two no longer line up.
//
// It also ends a buffer's blank state and a wipe in progress. Under a mode the
// model does not follow, the change is not placed as the real terminal places
// it, so the content is in doubt past the mode's end (#149 review, N1).
func (s *Screen) edit() {
	b := s.buf()
	if s.cur.rowKnown && !s.anchored[b] || s.irm || s.lnm || s.origin || s.region {
		s.doubt()
	}
	s.blank[b], s.wipeOn = false, false
}

func (s *Screen) eraseDisplay(m int) {
	g := s.grid()
	switch m {
	case 0:
		// From the model's home only with the row known: with rows relative,
		// the real cursor may be lower, and rows above it keep their text
		// (#149 review, N2).
		if s.cur.row == 0 && s.cur.col == 0 && !s.cur.wrapNext && s.cur.rowKnown {
			s.fullClear()
			return
		}
		s.edit()
		s.clearCells(s.cur.row, s.cur.col, s.cols)
		for r := s.cur.row + 1; r < s.rows; r++ {
			clear(g[r])
		}
	case 1:
		s.edit()
		for r := 0; r < s.cur.row; r++ {
			clear(g[r])
		}
		s.clearCells(s.cur.row, 0, s.cur.col+1)
	case 2:
		s.fullClear()
	case 3:
		// The scrollback only.
	default:
		s.lost = true
	}
}

// fullClear blanks the screen in use: the content no longer depends on where
// the CLI started. With the cursor's row real, rows become real.
//
// It ends the buffer's doubt over its content, not lost or a mode: a clear
// blanks cells, and leaves modes and the cursor's visibility as they were.
func (s *Screen) fullClear() {
	b := s.buf()
	s.clearGrid(s.grid())
	s.wipeOn = false
	if s.bad || s.region || s.origin {
		// Under a scroll region or origin mode the model's cells and rows are
		// not the real ones; a clear does not make them so (#149 review, N1).
		s.doubt()
		return
	}
	s.unsure[b], s.blank[b] = false, true
	if s.cur.rowKnown {
		s.anchored[b] = true
	} else {
		s.maxRow = s.cur.row
	}
}

// setMode sets a mode the model does not follow. When it goes on or off, the
// content, and the cursor's place, may differ from the real screen's until a
// full clear with the mode off (#149 review, N1); a wipe in progress ends.
func (s *Screen) setMode(m *bool, on bool) {
	if *m == on {
		return
	}
	*m = on
	s.doubt()
	s.wipeOn = false
}

// startWipe begins following a redraw from home (wipe): the cursor went to
// the real top row of a buffer whose rows are relative.
func (s *Screen) startWipe() {
	if s.anchored[s.buf()] || s.region || s.origin {
		return
	}
	clear(s.wipeRows)
	s.wipeOn, s.wipeN = true, 0
}

func (s *Screen) eraseLine(m int) {
	switch m {
	case 0:
		s.edit()
		s.clearCells(s.cur.row, s.cur.col, s.cols)
	case 1:
		s.edit()
		s.clearCells(s.cur.row, 0, s.cur.col+1)
	case 2:
		wiping := s.wipeOn && s.cur.rowKnown
		s.edit()
		s.clearCells(s.cur.row, 0, s.cols)
		if wiping {
			// Every real row erased with nothing else drawn between is the
			// whole real screen erased: as a full clear (T7016 review).
			s.wipeOn = true
			if !s.wipeRows[s.cur.row] {
				s.wipeRows[s.cur.row] = true
				s.wipeN++
			}
			if s.wipeN == s.rows {
				s.fullClear()
			}
		}
	default:
		s.lost = true
	}
}

// clearCells blanks columns [from, to) of row r, and any wide character cut.
func (s *Screen) clearCells(r, from, to int) {
	if from >= to {
		return
	}
	row := s.grid()[r]
	s.work += to - from
	clear(row[from:to])
	// Only the edges can cut a wide character.
	if from > 0 && row[from-1].Wide {
		row[from-1] = Cell{}
	}
	if to < len(row) && row[to].Cont {
		row[to] = Cell{}
	}
}

func (s *Screen) deleteChars(n int) {
	s.edit()
	s.cur.wrapNext = false
	row := s.grid()[s.cur.row]
	n = min(n, s.cols-s.cur.col)
	copy(row[s.cur.col:], row[s.cur.col+n:])
	clear(row[s.cols-n:])
	s.fixWide(s.cur.row)
}

func (s *Screen) insertChars(n int) {
	s.edit()
	s.cur.wrapNext = false
	row := s.grid()[s.cur.row]
	n = min(n, s.cols-s.cur.col)
	copy(row[s.cur.col+n:], row[s.cur.col:])
	clear(row[s.cur.col : s.cur.col+n])
	s.fixWide(s.cur.row)
}

// unsplit blanks the other half of a wide character at column c of row, as c
// is about to be drawn over.
func unsplit(row []Cell, c int) {
	switch {
	case row[c].Cont && c > 0:
		row[c-1] = Cell{}
	case row[c].Wide && c+1 < len(row):
		row[c+1] = Cell{}
	}
}

// fixWide blanks the halves of wide characters that lost their other half.
func (s *Screen) fixWide(r int) {
	row := s.grid()[r]
	s.work += len(row)
	for c := range row {
		switch {
		case row[c].Wide && (c+1 >= len(row) || !row[c+1].Cont):
			row[c] = Cell{}
		case row[c].Cont && (c == 0 || !row[c-1].Wide):
			row[c] = Cell{}
		}
	}
}

// text takes a printable byte: ASCII, or part of a UTF-8 sequence.
func (s *Screen) text(c byte) {
	switch {
	case s.utfNeed > 0 && c&0xc0 == 0x80:
		s.utf[s.utfN] = c
		s.utfN++
		if s.utfN == s.utfNeed {
			r := decode(s.utf[:s.utfN])
			s.utfN, s.utfNeed = 0, 0
			if r < 0 {
				s.doubt()
				r = 0xfffd
			}
			s.print(r)
		}
		return
	case s.utfNeed > 0:
		s.badUTF()
	}
	switch {
	case c < 0x80:
		s.print(rune(c))
	case c&0xe0 == 0xc0:
		s.utf[0], s.utfN, s.utfNeed = c, 1, 2
	case c&0xf0 == 0xe0:
		s.utf[0], s.utfN, s.utfNeed = c, 1, 3
	case c&0xf8 == 0xf0:
		s.utf[0], s.utfN, s.utfNeed = c, 1, 4
	default:
		s.doubt() // terminals differ on invalid UTF-8
		s.print(0xfffd)
	}
}

// badUTF ends a UTF-8 sequence cut short.
func (s *Screen) badUTF() {
	s.utfN, s.utfNeed = 0, 0
	s.doubt()
	s.print(0xfffd)
}

// decode returns the rune of a whole UTF-8 sequence, or -1 for an overlong
// form, a surrogate, or one past U+10FFFF.
func decode(b []byte) rune {
	var r rune
	switch len(b) {
	case 2:
		r = rune(b[0]&0x1f)<<6 | rune(b[1]&0x3f)
		if r < 0x80 {
			return -1
		}
	case 3:
		r = rune(b[0]&0x0f)<<12 | rune(b[1]&0x3f)<<6 | rune(b[2]&0x3f)
		if r < 0x800 || r >= 0xd800 && r <= 0xdfff {
			return -1
		}
	case 4:
		r = rune(b[0]&0x07)<<18 | rune(b[1]&0x3f)<<12 | rune(b[2]&0x3f)<<6 | rune(b[3]&0x3f)
		if r < 0x10000 || r > 0x10ffff {
			return -1
		}
	}
	return r
}

// print draws r at the cursor.
func (s *Screen) print(r rune) {
	if r >= 0x80 && r <= 0x9f {
		s.lost = true // a C1 control: terminals differ in UTF-8, and it may be CSI
		return
	}
	if g := s.cur.g[s.cur.shift]; g != 'B' {
		s.doubt() // a character set other than ASCII maps characters
	}
	w := Width(r)
	if w == 0 {
		return // a combining mark joins the character before
	}
	s.edit()
	if s.cur.wrapNext && s.autowrap {
		s.cur.col = 0
		s.lineFeed()
	}
	if w == 2 && s.cur.col == s.cols-1 {
		if !s.autowrap || s.cols < 2 {
			s.doubt()
			return
		}
		s.clearCells(s.cur.row, s.cur.col, s.cols)
		s.cur.col = 0
		s.lineFeed()
	}
	row := s.grid()[s.cur.row]
	c := s.cur.col
	unsplit(row, c)
	if w == 2 {
		unsplit(row, c+1)
	}
	row[c] = Cell{R: r, Wide: w == 2, Faint: s.cur.faint}
	if w == 2 {
		row[c+1] = Cell{Cont: true, Faint: s.cur.faint}
	}
	if c+w >= s.cols {
		s.cur.col = s.cols - 1
		s.cur.wrapNext = s.autowrap
	} else {
		s.cur.col = c + w
		s.cur.wrapNext = false
	}
}

// Snapshot is a copy of the screen at one moment.
type Snapshot struct {
	Rows, Cols int
	// Row and Col are the cursor's; Visible, whether it is shown.
	Row, Col int
	Visible  bool
	// Alt: the alternate screen is on.
	Alt bool
	// Unsure: the copy may differ from the real screen (see the package
	// doc), or a sequence is cut in the middle.
	Unsure bool
	cells  [][]Cell
}

// Snapshot copies the screen in use.
func (s *Screen) Snapshot() *Snapshot {
	g, b := s.grid(), s.buf()
	cells := make([][]Cell, len(g))
	for r := range g {
		cells[r] = append([]Cell(nil), g[r]...)
	}
	return &Snapshot{
		Rows: s.rows, Cols: s.cols, Row: s.cur.row, Col: s.cur.col, Visible: s.visible, Alt: s.alt,
		Unsure: s.unsure[b] || s.lost || s.bad || s.irm || s.lnm || s.origin || s.region ||
			s.cur.rowKnown && !s.anchored[b] || s.st != stGround || s.utfNeed > 0,
		cells: cells,
	}
}

// Cell returns the cell at row r, column c, or a blank one outside the
// screen.
func (p *Snapshot) Cell(r, c int) Cell {
	if r < 0 || r >= len(p.cells) || c < 0 || c >= len(p.cells[r]) {
		return Cell{}
	}
	return p.cells[r][c]
}

// Line returns row r's text: a blank as a space, a wide character once.
func (p *Snapshot) Line(r int) string {
	if r < 0 || r >= len(p.cells) {
		return ""
	}
	var b strings.Builder
	for _, c := range p.cells[r] {
		switch {
		case c.Cont:
		case c.R == 0:
			b.WriteByte(' ')
		default:
			b.WriteRune(c.R)
		}
	}
	return b.String()
}
