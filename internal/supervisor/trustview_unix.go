//go:build unix

package supervisor

import (
	"fmt"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
	"github.com/congdoe/aggy/internal/trustlist"
)

// /aggy trust (T6977): this folder's trust list on aggy's own screen, drawn as
// a menu is, with its rows ticked in place. Only the keyboard opens it (the
// command is keyboard only) and only keys answer it: a paste is dropped before
// it (viewPaste), and keys that arrive within checkTypeAhead of it opening
// were typed before it, so they do nothing.

// checkTypeAhead is how soon after the list opens a key counts as typed
// before it, as the trust list before a launch drops a line typed ahead; a
// variable for tests.
var checkTypeAhead = 150 * time.Millisecond

// checkList is a trust list being answered.
type checkList struct {
	lines  []string // what it says first
	st     *trustlist.State
	at     int // the highlighted item: 0 is All, then each row
	opened time.Time
}

// view is the list's screen: title, what it says, the rows, and the keys.
func (c *checkList) view(title string, rows int) []string {
	out := []string{"aggy: " + title, ""}
	out = append(out, c.lines...)
	mark := fmt.Sprintf("%2d ", c.at+1)
	for _, l := range c.st.Lines() {
		if strings.HasPrefix(l, mark) {
			l = ">" + l
		} else {
			l = " " + l
		}
		out = append(out, l)
	}
	for len(out) < rows-2 {
		out = append(out, "")
	}
	return append(out, "", "Space or a number ticks; Enter confirms; n: not now; x: never for this folder; Esc goes back to the CLI.")
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
	if time.Since(c.opened) < checkTypeAhead {
		return false // typed before the list was there
	}
	switch {
	case k == keyClose || r == 'q' || r == 'n':
		// Esc, or not now: nothing is recorded, and a start asks again.
		s.closeView()
		return true
	case k == keyUp || r == 'k':
		c.at = max(c.at-1, 0)
	case k == keyDown || r == 'j':
		c.at = min(c.at+1, len(c.st.Rows))
	case r == ' ':
		c.st.Toggle(c.at + 1)
	case r >= '1' && r <= '9':
		c.st.Toggle(int(r - '0'))
	case k == keyEnter:
		s.answerTrust(m.title, c.st.Ticked(), trustlist.Confirm)
		return true
	case r == 'x':
		s.answerTrust(m.title, c.st.Ticked(), trustlist.Never)
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
