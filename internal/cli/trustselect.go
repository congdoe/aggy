package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/trustlist"
)

// The trust list as a select (T6981): at a first start, and at aggy trust
// <dir>, a terminal that can take raw mode gets the list as Claude Code's
// pickers are, drawn in place on the normal screen (no CLI's screen exists
// yet), redrawn on each key, and left in the scrollback once, as answered.
// Up/Down (k/j, Ctrl-P/Ctrl-N) move, Space ticks, Enter confirms, a number
// ticks its row, n or Esc is not now, x is never, Ctrl-C stops aggy
// (trustlist.Select). Raw mode is the supervisor's (supervisor.InPlace),
// the only package that may set it; aggy reads stdin itself, so nothing is
// left reading once the list is gone. A terminal that cannot (TERM unset or
// dumb, raw mode refused, not a terminal) gets the typed list as before.

// inPlaceTerm is the terminal in raw mode.
type inPlaceTerm interface {
	Size() (rows, cols int)
	Restore()
}

// startInPlace puts the terminal in raw mode; interruptAggy ends aggy on
// Ctrl-C. Tests replace them.
var (
	startInPlace = func(in io.Reader, cleanup, resized func()) (inPlaceTerm, error) {
		f, ok := in.(*os.File)
		if !ok {
			return nil, errors.New("not a terminal")
		}
		return supervisor.StartInPlace(f, cleanup, resized)
	}
	interruptAggy = supervisor.Interrupt
	// selectNow is the select's clock, for its typing guard.
	selectNow = time.Now
)

// selectTerminal reports whether the select can be drawn; a test replaces it.
var selectTerminal = trustSelectTerminal

// trustSelectTerminal reports whether the select can be drawn: in and out
// terminals, and a TERM that can move the cursor.
func trustSelectTerminal(in io.Reader, out io.Writer) bool {
	fin, ok := in.(*os.File)
	fout, ok2 := out.(*os.File)
	return ok && ok2 && isTerminal(fin) && isTerminal(fout) && cursorTerm(os.Getenv)
}

// cursorTerm reports whether TERM names a terminal that moves the cursor:
// set, and not dumb.
func cursorTerm(getenv func(string) string) bool {
	t := getenv("TERM")
	return t != "" && t != "dumb"
}

// The escape sequences the select writes.
const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	// Bracketed paste on, so a paste reaches the list marked and answers
	// nothing; off again after, as the shell leaves it while a command runs.
	bracketOn  = "\x1b[?2004h"
	bracketOff = "\x1b[?2004l"
	eraseDown  = "\x1b[J"
	clearAll   = "\x1b[H\x1b[2J"
)

// trustScreen is the select on the terminal. mu guards the drawing: a key
// and a resize (from a signal) draw from different goroutines.
type trustScreen struct {
	mu     sync.Mutex
	w      io.Writer
	sel    *trustlist.Select
	header string
	size   func() (rows, cols int)
	drawn  int // rows the list took when last drawn
	closed bool
}

// crlf makes s's line ends raw mode's, which moves to the next line only.
func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// frame is the list as it fits the terminal now, one row left free so the
// drawing never fills the screen and a redraw reaches its top.
func (t *trustScreen) frame() []string {
	rows, cols := 0, 0
	if t.size != nil {
		rows, cols = t.size()
	}
	if rows <= 0 || cols <= 0 {
		rows, cols = 24, 80
	}
	return t.sel.Frame(cols, rows-1)
}

// erase takes the last drawing off: up to its first row, and everything
// below.
func (t *trustScreen) erase(b *strings.Builder) {
	if t.drawn > 0 {
		b.WriteString("\r")
		if t.drawn > 1 {
			fmt.Fprintf(b, "\x1b[%dA", t.drawn-1)
		}
		b.WriteString(eraseDown)
	}
	t.drawn = 0
}

// redraw draws the list again in place. The caller holds mu.
func (t *trustScreen) redraw() {
	if t.closed {
		return
	}
	var b strings.Builder
	t.erase(&b)
	lines := t.frame()
	b.WriteString(strings.Join(lines, "\r\n"))
	t.drawn = len(lines)
	_, _ = io.WriteString(t.w, b.String())
}

// start writes the header and draws the list. It reports false, writing
// nothing, when the select was already put away (a signal came between raw
// mode and the first draw).
func (t *trustScreen) start() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	_, _ = io.WriteString(t.w, hideCursor+bracketOn+crlf(t.header))
	t.redraw()
	return true
}

// resized draws the header and the list again from the top of the screen:
// a terminal may have reflowed the old drawing, so its rows are not known.
func (t *trustScreen) resized() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	_, _ = io.WriteString(t.w, clearAll+crlf(t.header))
	t.drawn = 0
	t.redraw()
}

// finish takes the drawing off and leaves the list as answered, plain, once
// in the scrollback; the cursor shows again and bracketed paste is off. It
// runs once, before the terminal is restored, on every way out.
func (t *trustScreen) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	var b strings.Builder
	t.erase(&b)
	for _, l := range t.sel.St.Lines() {
		b.WriteString(l + "\r\n")
	}
	b.WriteString(showCursor + bracketOff)
	_, _ = io.WriteString(t.w, b.String())
}

// selectTrustList answers l as a select on the terminal in and out. ok is
// false, with nothing written, when raw mode cannot be had.
func selectTrustList(l *trustListAt, header string, in io.Reader, out io.Writer) (trustlist.Choice, bool) {
	t := &trustScreen{w: out, sel: trustlist.NewSelect(l.st), header: header}
	t.mu.Lock() // a resize waits for the size to be known
	raw, err := startInPlace(in, t.finish, t.resized)
	if err == nil {
		t.size = raw.Size
	}
	t.mu.Unlock()
	if err != nil {
		return trustlist.Nothing, false
	}
	defer raw.Restore() // a panic restores too
	c := runTrustSelect(t, in)
	raw.Restore()
	if c == trustlist.Interrupt {
		interruptAggy()
		return trustlist.NotNow, true // only if aggy is still here
	}
	return c, true
}

// runTrustSelect draws the select and reads keys from r until one ends it.
// Reads that come within typeAhead of the list appearing were typed before
// it, and answer nothing (an Enter typed ahead never confirms); they still
// go through the decoder, so a paste begun then is still seen. The end of
// input, or a read that fails, is not now.
func runTrustSelect(t *trustScreen, r io.Reader) trustlist.Choice {
	if !t.start() {
		return trustlist.NotNow
	}
	shown := selectNow()
	t.mu.Lock()
	t.sel.Shown(shown)
	t.mu.Unlock()
	var d trustlist.Decoder
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			now := selectNow()
			keys := d.Feed(buf[:n])
			if now.Sub(shown) < typeAhead && len(keys) > 0 && keys[0].Key != trustlist.KeyInterrupt {
				// Typed before the list was there: nothing, and what follows
				// is typing until a pause.
				t.mu.Lock()
				t.sel.Typed(now)
				t.redraw()
				t.mu.Unlock()
				keys = nil
			}
			for _, k := range keys {
				t.mu.Lock()
				c := t.sel.Press(k, now)
				if c == trustlist.Toggled || c == trustlist.Nothing {
					t.redraw() // a note may have come or gone
				}
				t.mu.Unlock()
				switch c {
				case trustlist.Confirm, trustlist.NotNow, trustlist.Never, trustlist.Interrupt:
					return c
				}
			}
		}
		if err != nil {
			return trustlist.NotNow
		}
	}
}
