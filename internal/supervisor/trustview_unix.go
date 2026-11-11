//go:build unix

package supervisor

import (
	"time"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
	"github.com/congdoe/aggy/internal/trustlist"
)

// /aggy trust (T6977): this folder's trust list on aggy's own screen, drawn as
// a menu is, with its rows ticked in place. Only the keyboard opens it (the
// command is keyboard only) and only keys answer it: a paste is dropped before
// it (viewPaste); keys that arrive within checkTypeAhead of it opening were
// typed before it, so they do nothing and start the select's typing guard;
// and a read of several keys, or of one aggy does not know, is typing
// (checkBurst). Esc, Ctrl-C, and the switch key always close it.

// checkTypeAhead is how soon after the list opens a key counts as typed
// before it, as the trust list before a launch drops a line typed ahead; a
// variable for tests.
var checkTypeAhead = 150 * time.Millisecond

// checkNow is the trust list's clock, for the select's typing guard; a test
// sets it.
var checkNow = time.Now

// checkList is a trust list being answered: the same select, keys, and look
// as the list at a first start (trustlist.Select, T6981), typing guard
// included.
type checkList struct {
	lines  []string // what it says first
	sel    *trustlist.Select
	opened time.Time
}

func newCheckList(lines []string, st *trustlist.State) *checkList {
	c := &checkList{lines: lines, sel: trustlist.NewSelect(st), opened: checkNow()}
	c.sel.Shown(c.opened)
	return c
}

// view is the list's screen: title, what it says, then the select in the
// rows left, scrolled to its cursor, with its keys last.
func (c *checkList) view(title string, rows, cols int) []string {
	out := []string{"aggy: " + title, ""}
	out = append(out, c.lines...)
	return append(out, c.sel.Frame(cols, max(rows-len(out), 1))...)
}

// checkBurst reports whether a read's keys may answer the list: one key, or
// the same arrow held. A burst of typing, or a read with a key aggy does not
// know, answers nothing, as at a first start (trustlist.Decoder).
func checkBurst(keys []viewKey, runes []rune) bool {
	for i, k := range keys {
		if k == keyNone && runes[i] == 0 {
			return false
		}
		if i > 0 && (k != keys[0] || k != keyUp && k != keyDown) {
			return false
		}
	}
	return true
}

// startTrust opens the trust list: read off the main loop, onto a page that
// becomes the list.
func (s *session) startTrust(c sessioncmd.Command) {
	if s.open {
		s.closePicker()
	}
	s.cmdSeq++
	pg := &page{title: c.Name, seq: s.cmdSeq, lines: []string{"Reading this folder's trust list..."}, busy: true}
	read, seq := s.o.TrustList, s.cmdSeq
	s.goSafe(func() {
		r := cmdResult{seq: seq}
		tl, err := read()
		if err != nil {
			r.out = "aggy cannot show this folder's trust list: " + err.Error()
		} else {
			r.trust = &tl
		}
		select {
		case s.cmdResults <- r:
		case <-s.quit:
		}
	})
	s.pk.list()
	s.pk.page = pg
	s.viewPaste = pasteFilter{}
	s.openPicker()
}

// checkKey takes one key at the trust list, and reports whether the list is
// gone.
func (s *session) checkKey(k viewKey, r rune) bool {
	m := s.pk.menu
	c := m.check
	now := checkNow()
	in := trustlist.Input{Key: trustlist.KeyRune, Rune: r}
	switch k {
	case keyUp:
		in.Key = trustlist.KeyUp
	case keyDown:
		in.Key = trustlist.KeyDown
	case keyEnter:
		in.Key = trustlist.KeyEnter
	case keyClose:
		// Esc, Ctrl-C, or the switch key always go back to the CLI, typing
		// or not: the page closes and nothing is recorded.
		in.Key = trustlist.KeyInterrupt
	case keyBackspace:
		in.Key = trustlist.KeyBack
	case keyNone:
		if r == 0 {
			in.Key = trustlist.KeyUnknown
		}
	default:
		return false
	}
	if now.Sub(c.opened) < checkTypeAhead && in.Key != trustlist.KeyInterrupt {
		c.sel.Typed(now) // typed before the list was there
		return false
	}
	switch c.sel.Press(in, now) {
	case trustlist.NotNow, trustlist.Interrupt:
		// Esc, or not now: nothing is recorded, and a start asks again.
		s.closeView()
		return true
	case trustlist.Confirm:
		s.answerTrust(m.title, c.sel.St.Ticked(), trustlist.Confirm)
		return true
	case trustlist.Never:
		s.answerTrust(m.title, c.sel.St.Ticked(), trustlist.Never)
		return true
	}
	return false
}

// answerTrust records the answer off the main loop, on a page that shows
// what it did.
func (s *session) answerTrust(title string, ticked []string, choice trustlist.Choice) {
	s.cmdSeq++
	seq, answer := s.cmdSeq, s.o.TrustAnswer
	s.pk.menu = nil
	s.pk.page = &page{title: title, seq: seq, lines: []string{"Saving this folder's answer..."}, busy: true}
	s.log(diag.Rec{Event: diag.EvCommandCaught, Attrs: map[string]any{"command": title, "via": "keys", "confirmed": true}})
	s.goSafe(func() {
		out := answer(ticked, choice)
		select {
		case s.cmdResults <- cmdResult{seq: seq, out: out}:
		case <-s.quit:
		}
	})
	s.redrawView()
}
