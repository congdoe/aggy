package claude

import (
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/screen"
)

// screenMarks are the marks of Claude Code's composer, as a live recording
// shows them (docs/verification.md, "Claude Code's screen", T7016): Claude
// Code 2.1.326, 2026-11-17, a bare pseudo-terminal of 30 by 100, one short
// model turn; the recording is testdata/fixtures/claude/screen. They hold at
// that version and at later patch versions of the same 2.1.x line
// (adapter.CoversLine), the rule internal/adapter/livechecks.go follows; any
// other version reads as adapter.ScreenOther, so a switch asks.
//
// What the recording shows: the composer is a line that starts with ❯ and a
// no-break space in columns 0 and 1, between two rules of ─ the full width of
// the screen, with the cursor shown in it. Empty, it holds Claude's
// placeholder (`Try "…"`) drawn faint (SGR 2), or nothing; typed text is not
// faint, and the cursor follows it. While a turn runs the footer below the
// lower rule says "esc to interrupt". The slash menu draws below the lower
// rule, with ❯ marking its selected row, over the footer. The /model dialog
// hides the cursor and draws over the composer. Bash mode (!) puts ! in
// column 0. The folder-trust dialog in a new folder marks its first option,
// "No, exit", with ❯ and a plain space, with no rule around it and the cursor
// hidden. Left on a fresh session's empty composer drew nothing at this
// version. The MCP approval, update, and terms dialogs were never recorded:
// they read as adapter.ScreenOther only if they lack these marks.
var screenMarks = struct {
	version      string
	prompt, sep  rune
	rule         rune
	textCol      int
	busy         string
	menuSelected rune
}{
	version:      "2.1.326",
	prompt:       '❯',
	sep:          0x00a0, // a no-break space
	rule:         '─',
	textCol:      2,
	busy:         "esc to interrupt",
	menuSelected: '❯',
}

// ClassifyScreen reads a copy of Claude Code's screen at version (an
// adapter.ScreenClassifier). It relies on the cursor's place and visibility
// and on the screen's structure, which output drawn as text (a folder name, a
// file's lines) does not move: see docs/design.md, section 5, "Reading the
// screen", for what a forged screen could cause.
func ClassifyScreen(version string, s *screen.Snapshot) adapter.ScreenState {
	m := screenMarks
	if s == nil || !adapter.CoversLine(m.version, version) || s.Alt || s.Unsure || !s.Visible {
		return adapter.ScreenOther
	}
	r := s.Row
	if r < 1 || r+1 >= s.Rows || s.Col < m.textCol {
		return adapter.ScreenOther
	}
	if c := s.Cell(r, 0); c.R != m.prompt || c.Wide {
		return adapter.ScreenOther
	}
	if s.Cell(r, 1).R != m.sep || !rule(s, r-1, m.rule) || !rule(s, r+1, m.rule) {
		return adapter.ScreenOther
	}
	busy := false
	for row := r + 2; row < s.Rows; row++ {
		line := s.Line(row)
		if strings.ContainsRune(line, m.menuSelected) {
			return adapter.ScreenOther // a menu below the composer
		}
		busy = busy || strings.Contains(line, m.busy)
	}
	if busy {
		return adapter.ScreenBusy
	}
	empty := true
	for c := m.textCol; c < s.Cols; c++ {
		if cell := s.Cell(r, c); !cell.Blank() && !cell.Faint {
			empty = false
			break
		}
	}
	switch {
	case !empty:
		return adapter.ScreenPromptText
	case s.Col == m.textCol:
		return adapter.ScreenPromptEmpty
	}
	return adapter.ScreenOther
}

// rule reports whether row r is ch across the whole screen.
func rule(s *screen.Snapshot, r int, ch rune) bool {
	for c := range s.Cols {
		if cell := s.Cell(r, c); cell.R != ch || cell.Wide || cell.Cont {
			return false
		}
	}
	return true
}
