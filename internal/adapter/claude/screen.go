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
// "No, exit", with ❯ and a plain space, with no rules directly above and below
// its option row (a rule tops the dialog), and the cursor hidden. Left on a fresh session's empty composer drew nothing at this
// version. The MCP approval, update, and terms dialogs were never recorded:
// they read as adapter.ScreenOther only if they lack these marks. The run was
// nested in another Claude Code session, which adds a warning line to the
// footer; the marks read nothing below the lower rule but the busy text and
// the menu's mark, so a normal run's footer (unverified) should not matter.
//
// The empty prompt also needs no ❯ in columns 0 to 2 above the composer
// (#149 review): before the first prompt nothing is echoed there. After one,
// its echo is, so the empty composer then reads as adapter.ScreenOther: the
// marks are tuned for the time before the first turn end, the only time the
// supervisor reads them.
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
	case s.Col != m.textCol:
		return adapter.ScreenOther
	}
	// Before the first turn end, with no Enter sent, nothing above the
	// composer carries the prompt mark: no prompt was echoed, and a ❯ there
	// is another selection (a menu or a dialog's choice) drawn above a
	// composer (#149 review). After a turn the echo of each prompt is there,
	// so the empty prompt then reads as Other: the classifier is for the
	// first turn only.
	for row := range r {
		for c := 0; c <= m.textCol; c++ {
			if s.Cell(row, c).R == m.prompt {
				return adapter.ScreenOther
			}
		}
	}
	return adapter.ScreenPromptEmpty
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
