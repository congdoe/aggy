package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/trustlist"
)

// reads gives one chunk per Read, as a terminal in raw mode gives a key per
// read, then the end of input.
type reads struct {
	chunks []string
	i      int
}

func (r *reads) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

// fakeRaw is raw mode as a test sees it: its size, and how often it was put
// back, the cleanup run first as supervisor.InPlace runs it.
type fakeRaw struct {
	rows, cols int
	cleanup    func()
	restored   int
}

func (f *fakeRaw) Size() (int, int) { return f.rows, f.cols }
func (f *fakeRaw) Restore() {
	if f.restored == 0 && f.cleanup != nil {
		f.cleanup()
	}
	f.restored++
}

// selectWorld is trustWorld with the select on: a terminal that takes raw
// mode at rows by cols, and Ctrl-C recorded instead of ending the test.
func selectWorld(t *testing.T, rows, cols int, installed ...string) (repo string, raw *fakeRaw, interrupted *int) {
	t.Helper()
	_, repo = trustWorld(t, installed...)
	raw, interrupted = &fakeRaw{rows: rows, cols: cols}, new(int)
	oldSel, oldStart, oldInt := selectTerminal, startInPlace, interruptAggy
	selectTerminal = func(io.Reader, io.Writer) bool { return true }
	startInPlace = func(_ io.Reader, cleanup, _ func()) (inPlaceTerm, error) {
		raw.cleanup = cleanup
		return raw, nil
	}
	interruptAggy = func() {
		if raw.restored == 0 {
			t.Error("Ctrl-C stopped aggy before the terminal was put back")
		}
		*interrupted++
	}
	oldNow := selectNow
	keyGap = time.Second
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	selectNow = func() time.Time { at = at.Add(keyGap); return at }
	t.Cleanup(func() { selectTerminal, startInPlace, interruptAggy, selectNow = oldSel, oldStart, oldInt, oldNow })
	return repo, raw, interrupted
}

// keyGap is how far apart selectWorld's clock puts the reads: a second, as a
// person answering, unless a test types faster.
var keyGap time.Duration

// startKeys is one on-screen start in dir, answered with keys, one per read.
func startKeys(dir string, cfg config.Config, keys ...string) string {
	var out bytes.Buffer
	trustAtStart(context.Background(), dir, cfg, os.Getenv, &reads{chunks: keys}, &out)
	return out.String()
}

// TestTrustSelectAtStart (T6981): the first start's list as a select: the
// arrows, Space, and Enter answer it; n, Esc, x, and Ctrl-C end it; every way
// out puts the terminal back once; the answer recorded is as the typed list
// records it.
func TestTrustSelectAtStart(t *testing.T) {
	cases := []struct {
		name   string
		keys   []string
		on     []string // the rows recorded ticked; nil with recorded false
		saved  bool     // an answer was recorded
		never  bool
		intr   bool
		saying string
	}{
		{"Enter at once grants nothing", []string{"\r"}, []string{}, true, false, false, "Saved this folder's answer"},
		{"Space at once ticks Claude Code", []string{" ", "\r"}, []string{"claude"}, true, false, false, "Saved"},
		{"Up to All, Space, Enter", []string{"\x1b[A", " ", "\r"}, []string{"claude", "opencode"}, true, false, false, "Saved"},
		{"Down, Space, Enter", []string{"\x1b[B", " ", "\r"}, []string{"opencode"}, true, false, false, "Saved"},
		{"Ctrl-N and Ctrl-P", []string{"\x0e", "\x10", "\x0e", " ", "\r"}, []string{"opencode"}, true, false, false, "Saved"},
		{"j, k, Space", []string{"j", "j", "k", " ", "\r"}, []string{"opencode"}, true, false, false, "Saved"},
		{"a typed number", []string{"3", "\r"}, []string{"opencode"}, true, false, false, "Saved"},
		{"n is not now", []string{"1", "n"}, nil, false, false, false, "Not now"},
		{"Esc is not now", []string{"1", "\x1b"}, nil, false, false, false, "Not now"},
		{"x then Enter is never", []string{"x", "\r"}, []string{}, true, true, false, "Never for this folder"},
		{"x alone is not never", []string{"x"}, nil, false, false, false, "Not now"},
		{"Ctrl-C stops", []string{"1", "\x03"}, nil, false, false, true, ""},
		{"Ctrl-C after a split sequence stops", []string{"\x1b[", "\x03"}, nil, false, false, true, ""},
		{"the end of input is not now", []string{"1"}, nil, false, false, false, "Not now"},
		{"a paste answers nothing", []string{"\x1b[200~1\r\x1b[201~", "\x1b"}, nil, false, false, false, "Not now"},
		// A paste, a burst, or a key aggy does not know is typing: the Enter
		// after it does not confirm.
		{"a paste across reads answers nothing", []string{"\x1b[200~1", "\r", "x\x1b[201~", "\r"}, nil, false, false, false, "Not now"},
		{"a burst answers nothing", []string{"1\r", "\r"}, nil, false, false, false, "Not now"},
		{"Alt-x is not Esc then x", []string{"\x1bx", "\r"}, nil, false, false, false, "Not now"},
		{"an escape sequence split", []string{"\x1b[", "A", " ", "\r"}, []string{"claude", "opencode"}, true, false, false, "Saved"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, raw, intr := selectWorld(t, 40, 120, "claude", "opencode")
			out := startKeys(repo, config.Default(), c.keys...)
			if raw.restored < 1 {
				t.Error("the terminal was not put back")
			}
			if (*intr == 1) != c.intr {
				t.Errorf("interrupted %d times", *intr)
			}
			a, saved := folderAnswer(t, repo)
			if saved != c.saved || saved && (!slices.Equal(a.On, c.on) || a.Never != c.never) {
				t.Errorf("answer %+v (saved %v), want on %q never %v saved %v", a, saved, c.on, c.never, c.saved)
			}
			if !strings.Contains(out, c.saying) {
				t.Errorf("output does not say %q:\n%q", c.saying, out)
			}
			// The scrollback keeps the list once, as answered: after the
			// last drawing is erased, the plain list, then the cursor back.
			end := strings.LastIndex(out, "\x1b[J")
			if end < 0 || strings.Count(out[end:], "All harnesses") != 1 || !strings.Contains(out[end:], showCursor+bracketOff) {
				t.Errorf("the list as answered is not left once:\n%q", out)
			}
		})
	}
}

// TestTrustSelectTyping (PR #140 review, H1): a prompt typed at the list a
// byte a read, at typing speed, answers nothing: no tick, no confirm, no
// never. Each fails at 4de725e.
func TestTrustSelectTyping(t *testing.T) {
	for _, text := range []string{"ok go\r", "see issue 4\r", "add tests\r", "fix it\r", "just do it\r", " hello\r", "2 files\r", "x\r"} {
		t.Run(strings.TrimSpace(text), func(t *testing.T) {
			repo, _, intr := selectWorld(t, 40, 120, "claude", "opencode")
			keyGap = 80 * time.Millisecond
			var keys []string
			for _, b := range text {
				keys = append(keys, string(b))
			}
			out := startKeys(repo, freeOn(), keys...)
			if a, saved := folderAnswer(t, repo); saved || *intr != 0 {
				t.Errorf("typing answered: %+v", a)
			}
			if !strings.Contains(out, "Not now") {
				t.Errorf("%q", out)
			}
		})
	}
}

// TestTrustSelectClosedFirst (PR #140 review, L3): a select put away before
// it was first drawn (a signal right after raw mode) draws nothing.
func TestTrustSelectClosedFirst(t *testing.T) {
	var out bytes.Buffer
	st := trustlist.NewState([]trustlist.Row{{ID: "claude", Label: "Claude Code", Group: trustlist.Harness}}, trustlist.Folder{}, false, nil)
	s := &trustScreen{w: &out, sel: trustlist.NewSelect(st), header: "header\n", closed: true}
	if s.start() || out.Len() != 0 {
		t.Errorf("drew %q", out.String())
	}
	if c := runTrustSelect(s, &reads{chunks: []string{" ", "\r"}}); c != trustlist.NotNow || out.Len() != 0 {
		t.Errorf("choice %d, drew %q", c, out.String())
	}
}

// TestTrustSelectTypedAhead (T6981): keys that arrive within the type-ahead
// window of the list appearing answer nothing: an Enter typed ahead never
// confirms, and a Space never ticks.
func TestTrustSelectTypedAhead(t *testing.T) {
	repo, raw, _ := selectWorld(t, 40, 120, "claude", "opencode")
	typeAhead = time.Hour
	out := startKeys(repo, config.Default(), " ", "1", "\r", "x")
	if _, saved := folderAnswer(t, repo); saved || !strings.Contains(out, "Not now") || raw.restored < 1 {
		t.Errorf("typed-ahead keys answered: saved %v restored %d\n%q", saved, raw.restored, out)
	}
}

// TestTrustSelectNoTerminal (T6981): with no person at a terminal the select
// is never tried and nothing is ticked.
func TestTrustSelectNoTerminal(t *testing.T) {
	repo, raw, _ := selectWorld(t, 40, 120, "claude")
	offerTerminal = func() bool { return false }
	selectTerminal = func(io.Reader, io.Writer) bool {
		t.Error("the select was tried with no one at a terminal")
		return true
	}
	if out := startKeys(repo, config.Default(), " ", "\r"); out != "" || raw.restored != 0 {
		t.Errorf("asked: %q", out)
	}
	if _, saved := folderAnswer(t, repo); saved {
		t.Error("an answer was recorded")
	}
}

// TestTrustSelectFallback (T6981): a terminal that refuses raw mode gets the
// typed list, numbers and an empty line, as before; so do TERM unset or dumb,
// and input or output that is no terminal.
func TestTrustSelectFallback(t *testing.T) {
	repo, _, _ := selectWorld(t, 40, 120, "claude", "opencode")
	startInPlace = func(io.Reader, func(), func()) (inPlaceTerm, error) { return nil, errors.New("no raw mode") }
	var out bytes.Buffer
	trustAtStart(context.Background(), repo, config.Default(), os.Getenv, strings.NewReader("2\n\n"), &out)
	if a, _ := folderAnswer(t, repo); !slices.Equal(a.On, []string{"claude"}) || !strings.Contains(out.String(), trustlist.Keys) || strings.Contains(out.String(), "\x1b") {
		t.Errorf("the typed list: %+v\n%q", a, out.String())
	}
	for _, term := range []string{"", "dumb"} {
		if cursorTerm(func(string) string { return term }) {
			t.Errorf("TERM %q takes the select", term)
		}
	}
	if !cursorTerm(func(string) string { return "xterm-256color" }) {
		t.Error("xterm does not take the select")
	}
	f, err := os.CreateTemp(t.TempDir(), "notty")
	if err != nil {
		t.Fatal(err)
	}
	selectTerminal = trustSelectTerminal
	for _, c := range []struct {
		in  io.Reader
		out io.Writer
	}{{strings.NewReader(""), &out}, {f, f}, {os.Stdin, &out}} {
		if selectTerminal(c.in, c.out) {
			t.Errorf("%T and %T take the select", c.in, c.out)
		}
	}
}

// panicWriter panics on its second write, as a draw might.
type panicWriter struct{ n int }

func (w *panicWriter) Write(p []byte) (int, error) {
	if w.n++; w.n == 2 {
		panic("draw")
	}
	return len(p), nil
}

// TestTrustSelectPanic (T6981): a panic while the select is up puts the
// terminal back.
func TestTrustSelectPanic(t *testing.T) {
	repo, raw, _ := selectWorld(t, 40, 120, "claude")
	func() {
		defer func() {
			if recover() == nil {
				t.Error("no panic")
			}
		}()
		trustAtStart(context.Background(), repo, config.Default(), os.Getenv, &reads{chunks: []string{" ", " "}}, &panicWriter{})
	}()
	if raw.restored == 0 {
		t.Error("a panic left the terminal raw")
	}
}

// TestTrustSelectDraw (T6981): each key redraws the list in place: back up
// to its first row, erase, draw; the drawing fits the terminal's width and
// leaves its last row free, with a window around the cursor when the list is
// taller.
func TestTrustSelectDraw(t *testing.T) {
	for _, c := range []struct{ rows, cols int }{{40, 120}, {12, 40}, {6, 30}} {
		repo, _, _ := selectWorld(t, c.rows, c.cols, "claude", "opencode", "hermes")
		out := startKeys(repo, freeOn(), "\x1b[B", "\x1b[B", "\x1b[B")
		draws := strings.Split(out, "\x1b[J")
		if len(draws) != 5 { // the first draw, three redraws, the list left
			t.Fatalf("%dx%d: %d draws:\n%q", c.rows, c.cols, len(draws)-1, out)
		}
		for i, d := range draws[1:4] {
			d = strings.TrimSuffix(d, "\r") // the next draw's return
			if i := strings.Index(d, "\r\x1b["); i >= 0 {
				d = d[:i]
			}
			lines := strings.Split(d, "\r\n")
			if len(lines) > c.rows-1 {
				t.Errorf("%dx%d draw %d: %d rows", c.rows, c.cols, i, len(lines))
			}
			for _, l := range lines {
				if len([]rune(l)) > c.cols-1 {
					t.Errorf("%dx%d draw %d: %q is wider than the terminal", c.rows, c.cols, i, l)
				}
			}
			if !strings.Contains(d, "> ") {
				t.Errorf("%dx%d draw %d shows no cursor:\n%s", c.rows, c.cols, i, d)
			}
		}
	}
}
