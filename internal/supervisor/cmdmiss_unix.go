//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// The miss record (T6982): when an Enter ends a line that reads as one of
// aggy's commands and the key catch lets it through, one record says why, in
// fixed reason names only, never the typed text. A line the tracker lost (an
// edit, an escape sequence, a paste) is recorded with no name, and only when
// its first key past any escape sequence was / or $, the start of a command.

// missWindow and missPerWindow bound the miss records: a person can press
// Enter often, and each record past the bound is counted in the next one.
const (
	missWindow    = time.Minute
	missPerWindow = 5
)

// noteMiss records an Enter in typed that ends a command line catchCommand did
// not take. It runs before the read is followed, so cmdLine is the line up to
// this read; the keys before the Enter in the same read are added to it here.
func (s *session) noteMiss(raw, typed []byte) {
	i := bytes.IndexByte(typed, '\r')
	if i < 0 || s.inPaste || s.draftPaste || i > 0 && (typed[i-1] == esc || typed[i-1] == '\\') {
		return // no Enter, or a newline in the composer: no line ends
	}
	before := typed[:i]
	lost := s.cmdBad || len(s.cmdLine)+len(before) > maxCmdLine
	for _, c := range before {
		lost = lost || c < 0x20 || c > 0x7e
	}
	var c sessioncmd.Command
	if !lost {
		var ok bool
		if c, ok = sessioncmd.Parse(s.o.Vendor, string(s.cmdLine)+string(before)); !ok {
			return
		}
	} else {
		head, ok := s.cmdHead, s.headSeen
		if len(s.cmdLine) == 0 && !s.cmdBad {
			head, ok = firstKey(before)
		}
		if !ok || head != '/' && head != '$' {
			return
		}
	}
	var why []string
	switch {
	case string(typed) != "\r":
		why = append(why, "enter_not_alone")
	case !plainEnter(raw):
		why = append(why, "enter_not_plain")
	}
	for _, d := range s.lineDoubts() {
		if d == "no_line" && len(before) > 0 {
			continue // the line is in this read: enter_not_alone says it
		}
		why = append(why, d)
	}
	if s.altBlocks() {
		why = append(why, "alt_screen")
	}
	if !lost {
		if name, _, _ := strings.Cut(c.Name, " "); s.o.CommandOff != nil && s.o.CommandOff(name) {
			why = append(why, "command_off")
		} else if !s.takes(c) {
			why = append(why, "not_taken")
		}
	}
	s.logMiss(c.Name, why)
}

// logMiss writes a miss record, at most missPerWindow a missWindow, on the
// terminal loop's path that never waits (diag.Queue.Log). name is "" for a
// line the tracker lost.
func (s *session) logMiss(name string, why []string) {
	now := time.Now()
	if now.Sub(s.missAt) >= missWindow {
		s.missAt, s.missN = now, 0
	}
	if s.missN >= missPerWindow {
		s.missSkipped++
		return
	}
	s.missN++
	if len(why) == 0 {
		why = []string{"unknown"}
	}
	attrs := map[string]any{"why": strings.Join(why, ","), "at_prompt": s.atPrompt, "turn_ran": s.turnRan, "held": s.held}
	if name != "" {
		attrs["command"] = name
	}
	if len(s.lineSeqs) > 0 {
		attrs["seqs"] = strings.Join(s.lineSeqs, ",")
	}
	if !s.lenKnown && s.unsettled != "" {
		attrs["unsettled"] = s.unsettled
	}
	if s.missSkipped > 0 {
		attrs["skipped"] = s.missSkipped
		s.missSkipped = 0
	}
	s.log(diag.Rec{Event: diag.EvCommandPassed, Attrs: attrs})
}

// hookMiss records a command a prompt hook reported that hookCommand refused
// for want of the Enter it goes with: why names the rule, and the parts of it
// that failed follow, each a fixed word.
func (s *session) hookMiss(c sessioncmd.Command, why []string) {
	for _, d := range []struct {
		on   bool
		name string
	}{
		{!s.enterAtPrompt, "enter_not_at_prompt"},
		{s.enterPick.bad, "keys_not_a_pick"},
		{s.afterEnter != 0, "keys_after_enter"},
		{time.Since(s.enterAt) >= promptBlockWait, "enter_too_old"},
		{s.held, "held"},
	} {
		if d.on {
			why = append(why, d.name)
		}
	}
	s.logMiss(c.Name, why)
}

// maxLineSeqs bounds the escape sequences kept for a miss record.
const maxLineSeqs = 4

// noteSeqs keeps, for a miss record, the kinds of the escape sequences typed
// on the line (seqKind), never their text. A read that starts a line starts
// the list again.
func (s *session) noteSeqs(typed []byte) {
	if len(s.cmdLine) == 0 && !s.cmdBad {
		s.lineSeqs = s.lineSeqs[:0]
	}
	for i := 0; i < len(typed); {
		if typed[i] != esc {
			i++
			continue
		}
		n := seqLen(typed[i:])
		if k := seqKind(typed[i : i+n]); len(s.lineSeqs) < maxLineSeqs && (len(s.lineSeqs) == 0 || s.lineSeqs[len(s.lineSeqs)-1] != k) {
			s.lineSeqs = append(s.lineSeqs, k)
		}
		i += n
	}
}

// seqKind names an escape sequence's kind without its text: its introducer, a
// CSI's private marker, and a final byte only from a fixed set (a lowercase
// letter outside it, which a person could type after a lone Esc, shows as ?).
func seqKind(seq []byte) string {
	if len(seq) < 2 {
		return "esc"
	}
	final := func(b byte, set string) string {
		if b >= 'A' && b <= 'Z' || strings.IndexByte(set, b) >= 0 {
			return string(b)
		}
		return "?"
	}
	switch seq[1] {
	case '[':
		k := "csi"
		if len(seq) > 2 && strings.IndexByte("<=>?", seq[2]) >= 0 {
			k += string(seq[2])
		}
		return k + final(seq[len(seq)-1], "~$cmnqtuy")
	case 'O':
		return "ss3" + final(seq[len(seq)-1], "")
	case ']':
		return "osc"
	case 'P':
		return "dcs"
	case '_':
		return "apc"
	}
	return "alt"
}

// firstKey is the first byte of b past any escape sequence: a terminal reply
// aggy did not know, a cursor key, or a paste's marker.
func firstKey(b []byte) (byte, bool) {
	for i := 0; i < len(b); {
		switch b[i] {
		case esc:
			i += seqLen(b[i:])
		case '\r', '\n':
			return 0, false
		default:
			return b[i], true
		}
	}
	return 0, false
}

// headByte follows one byte the person sent for cmdHead: the first byte of
// the line past any escape sequence. A line that has had no byte yet (cmdLine
// empty and not bad) starts it again.
func (s *session) headByte(c byte) {
	if len(s.cmdLine) == 0 && !s.cmdBad {
		s.headSeen, s.headSeq = false, 0
	}
	if s.headSeen {
		return
	}
	switch s.headSeq {
	case 1: // after ESC
		switch c {
		case '[':
			s.headSeq = 2
		case 'O':
			s.headSeq = 3
		case esc:
		default:
			s.headSeq = 0
		}
		return
	case 2: // in a CSI, to its final byte
		if c >= 0x40 && c <= 0x7e {
			s.headSeq = 0
		}
		return
	case 3: // SS3's one byte
		s.headSeq = 0
		return
	}
	switch c {
	case esc:
		s.headSeq = 1
	case '\r', '\n':
	default:
		s.cmdHead, s.headSeen = c, true
	}
}
