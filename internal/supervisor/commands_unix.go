//go:build unix

package supervisor

import (
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// Commands inside a session (T6908, docs/design.md section 5): a command typed
// in full at the prompt is caught from the keystrokes. aggy holds back its
// Enter, clears the composer with Ctrl+U, and acts, so nothing reaches the CLI
// and no model turn is spent. That Ctrl+U clears the composer line is
// unverified on every CLI (the composer tracker assumes it already, send).
//
// Only a line the composer tracker knows whole is taken: typed since an Enter
// that left the composer empty, as printable keys, with no paste, no cursor
// key, no edit, no history recall, no newline, and no Esc; and only on an Enter
// that comes alone in its read, as a person's does. Anything else, a paste
// that ends in an Enter included, goes to the CLI as typed, where a miss is
// harmless: the CLI refuses an unknown command, or a skill tells the model to
// change nothing.

// catchCommand takes a read that is one plain Enter (raw, as it came, and
// typed, as legacyKeys gave it) ending a command the tracker knows whole. It
// reports whether it took it: then nothing of the read reaches the CLI.
func (s *session) catchCommand(raw, typed []byte) bool {
	if string(typed) != "\r" || !plainEnter(raw) || !s.lineKnown() {
		return false
	}
	c, ok := sessioncmd.Parse(s.o.Vendor, string(s.cmdLine))
	if !ok || !s.takes(c) {
		return false
	}
	if s.o.Vendor == adapter.Claude || s.o.Vendor == adapter.Grok {
		// Claude and Grok render inline, so on the alternate screen the keys go
		// to an editor or a pager the CLI opened, where / searches.
		s.outMu.Lock()
		alt := s.alt.on
		s.outMu.Unlock()
		if alt {
			return false
		}
	}
	clear := []byte{ctrlU}
	if raw[0] == esc {
		// The terminal reports keys as CSI u: Ctrl+U the same way (unverified
		// that each CLI takes it so; its legacy byte may also do).
		clear = []byte("\x1b[117;5u")
		s.enterRelease = true // the release that follows is the held Enter's
	}
	s.toChild.push(clear)
	s.lastIn = time.Now()
	s.inputSinceTurn = s.lineInput // the line and its Ctrl+U together typed nothing
	s.typedSince = false
	s.emptyComposer()
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": c.Name, "via": "keys"}})
	s.runCommand(c, true)
	return true
}

// lineKnown reports whether the composer holds exactly cmdLine: typed since an
// Enter that sent what was there (or since launch), as printable keys only.
func (s *session) lineKnown() bool {
	switch {
	case len(s.cmdLine) == 0, s.cmdBad, !s.lenKnown, s.draftBlind, s.draftPaste, s.inPaste, s.pastePop:
		return false
	case s.cleared, s.escDoubt, s.escEmpty, s.escTwice, s.moved, s.askOpen(), s.exitGap:
		return false
	}
	return s.compLen == len(s.cmdLine) && s.draftLen == len(s.cmdLine)
}

// plainEnter reports whether raw is one press of Enter with no modifier: the
// byte, or its kitty or modifyOtherKeys report (Caps and Num Lock allowed). A
// Shift+Enter or an Alt+Enter is a newline in some composers.
func plainEnter(raw []byte) bool {
	if string(raw) == "\r" {
		return true
	}
	r, ok := parseCSIu(raw)
	return ok && r.n == len(raw) && (r.code == 13 || r.code == kittyKeypadCR) && (r.mods-1)&^modLocks == 0 && (r.event == 0 || r.event == 1)
}

// enterReleaseOnly reports whether raw is one release of Enter, as kitty's
// report-all-events mode sends after a press.
func enterReleaseOnly(raw []byte) bool {
	r, ok := parseCSIu(raw)
	return ok && r.n == len(raw) && (r.code == 13 || r.code == kittyKeypadCR) && r.event == 3
}

// emptyComposer records that the composer is empty, as after a clean send.
func (s *session) emptyComposer() {
	s.compLen, s.lenKnown, s.popup, s.cleared, s.hadAt, s.moved, s.pastePop = 0, true, false, false, false, false, false
	s.slashOnly, s.slashEntered, s.slashLine = false, false, false
	s.draftLen, s.draftBlind, s.draftHigh = 0, false, false
	s.cmdLine, s.cmdBad = s.cmdLine[:0], false
	s.toldDirty = false
	s.updateDirty()
}

// takes reports whether this session acts on c. Each kind is taken only where
// it has something to act with.
func (s *session) takes(c sessioncmd.Command) bool {
	switch c.Kind {
	case sessioncmd.Picker, sessioncmd.Switch:
		return true
	}
	return false
}

// runCommand acts on a command, from the keyboard (keyed) or a hook.
func (s *session) runCommand(c sessioncmd.Command, keyed bool) {
	switch c.Kind {
	case sessioncmd.Picker:
		if s.open {
			s.closePicker()
		}
		s.openList()
	case sessioncmd.Switch:
		if s.open {
			s.closePicker()
		}
		s.pickSwitch(Choice{Vendor: c.Vendor, Brief: c.Brief, New: c.New, Model: c.Model}, keyed)
	}
}
