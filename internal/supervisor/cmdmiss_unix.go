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
		if !ok || head != '/' && head != '$' || s.menuPickOfOther() {
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
		s.logMiss(&s.keyMisses, diag.EvCommandPassed, s.missAttrs(c.Name, why))
		return
	}
	// A lost line may be a pick from the CLI's menu that its prompt hook
	// catches a moment later, so its record waits (flushMiss) and goes if the
	// hook takes the command for this Enter (#142 review).
	s.flushMiss(true)
	s.heldMiss = &heldMiss{attrs: s.missAttrs("", why), at: time.Now()}
}

// heldMiss is a record of a lost line, held for the prompt hook (noteMiss).
type heldMiss struct {
	attrs map[string]any
	at    time.Time
}

// missHold is how long a held record waits for the prompt hook unless a key
// comes first: past aggy hook's own answer time.
const missHold = promptAnswerWait

// flushMiss writes the held record: always when force (a key came, or the
// session ends), else once missHold has passed.
func (s *session) flushMiss(force bool) {
	if s.heldMiss == nil || !force && time.Since(s.heldMiss.at) < missHold {
		return
	}
	attrs := s.heldMiss.attrs
	s.heldMiss = nil
	s.logMiss(&s.keyMisses, diag.EvCommandPassed, attrs)
}

// menuPickOfOther reports whether the line just ended was picked from the CLI's
// menu with keys that start none of aggy's commands (/st for /status), so
// it was the CLI's own command, not aggy's (#142 review).
func (s *session) menuPickOfOther() bool {
	p := s.pick
	if p.bad || !p.menu || len(p.text) < 2 || !strings.ContainsRune(sessioncmd.Prefixes(s.o.Vendor), rune(p.text[0])) {
		return false
	}
	word, _, _ := strings.Cut(string(p.text[1:]), " ")
	for _, w := range []string{"switch", "borrow", "inbox", "aggy"} {
		if strings.HasPrefix(w, word) {
			return false
		}
	}
	return true
}

// missBudget bounds one kind of miss record: missPerWindow a missWindow, the
// rest counted in the next one.
type missBudget struct {
	at         time.Time
	n, skipped int
}

// missAttrs is a miss record's attrs, as the session stands now. name is ""
// for a line the tracker lost.
func (s *session) missAttrs(name string, why []string) map[string]any {
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
	return attrs
}

// logMiss writes a miss record within b, on the terminal loop's path that never
// waits (diag.Queue.Log). The key catch's and the prompt hook's records each
// have their own budget, so forged hook requests cannot use up the other.
func (s *session) logMiss(b *missBudget, ev diag.Event, attrs map[string]any) {
	now := time.Now()
	if now.Sub(b.at) >= missWindow {
		b.at, b.n = now, 0
	}
	if b.n >= missPerWindow {
		b.skipped++
		return
	}
	b.n++
	if b.skipped > 0 {
		attrs["skipped"] = b.skipped
		b.skipped = 0
	}
	s.log(diag.Rec{Event: ev, Attrs: attrs})
}

// hookMiss records a command a prompt hook reported that hookCommand did not
// run but blocked (blockHook): why (hookDoubts) names the rule, and the parts
// of it that failed follow, each a fixed word.
func (s *session) hookMiss(c sessioncmd.Command, why []string) {
	s.flushMiss(true)
	attrs := s.missAttrs(c.Name, why)
	attrs["via"] = "hook"
	if s.enterPick.bad && s.enterPick.why != "" {
		attrs["pick"] = s.enterPick.why
	}
	s.logMiss(&s.hookMisses, diag.EvCommandBlocked, attrs)
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

// keyFinals are the final bytes seqKind keeps: those of the keys (arrows, Home,
// End, F1 to F4, the ~ keys, CSI u). Any other, a letter a person typed after a
// lone Esc and [ among them, shows as ? (#142 review).
const keyFinals = "ABCDHFPQRS~u"

// seqKind names an escape sequence's kind without its text: its introducer, a
// CSI's private marker, and its final byte when it is a key's (keyFinals).
func seqKind(seq []byte) string {
	if len(seq) < 2 {
		return "esc"
	}
	final := func(b byte) string {
		if strings.IndexByte(keyFinals, b) >= 0 {
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
		return k + final(seq[len(seq)-1])
	case 'O':
		return "ss3" + final(seq[len(seq)-1])
	case ']':
		return "osc"
	case 'P':
		return "dcs"
	case '_':
		return "apc"
	}
	return "alt"
}

// firstKey is the first byte of b past any escape sequence (a terminal reply
// aggy did not know, a cursor key, or a paste's marker) and any control key or
// Backspace, which types no text.
func firstKey(b []byte) (byte, bool) {
	for i := 0; i < len(b); {
		switch c := b[i]; {
		case c == esc:
			i += seqLen(b[i:])
		case c == '\r' || c == '\n':
			return 0, false
		case c < 0x20 || c == 0x7f:
			i++
		default:
			return c, true
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
	switch {
	case c == esc:
		s.headSeq = 1
	case c < 0x20 || c == 0x7f:
		// A control key or a Backspace types no text: a Ctrl+U or a Backspace
		// before the / left the line unrecorded (the owner's run, T6982).
	default:
		s.cmdHead, s.headSeen = c, true
	}
}
