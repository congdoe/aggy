package trustlist

import (
	"bytes"
	"slices"
)

// Decoder reads a terminal's bytes in raw mode, one read at a time, as keys
// at the select (T6981). Its state carries from one read to the next, so a
// bracketed paste, or an escape sequence split across reads, is still seen
// whole.
//
// What a read may answer, on the safe side:
//   - Ctrl-C in a read ends the list, whatever else the read holds.
//   - A bracketed paste answers nothing, its markers found across reads, and
//     neither does any key in the same read as part of one: the read comes
//     back as one KeyUnknown, which the select takes as typing.
//   - A read holding a byte or an escape sequence that is not a known key is
//     one KeyUnknown, not read byte by byte: an Alt-x (ESC x), or a sequence
//     aggy does not know, is never Esc and then x.
//   - A read of several keys answers only when they are all the same arrow
//     (a held key): a burst of typing, a paste with no brackets among them,
//     is one KeyUnknown.
//   - An escape sequence cut off at the end of a read is held for the next.
//     A read that is Esc alone is Esc: the terminal writes a key's sequence
//     whole, and should one be split just after its Esc, the list ends as not
//     now, which records nothing, and the rest of the sequence is never read
//     as keys at it.
//
// TODO(T6954): that rest, and anything else typed once the list has ended,
// stays in the terminal's input and reaches the CLI started next as typing.
// Waiting a moment after a lone Esc, or discarding what is left, needs a read
// that can time out or a flush of the terminal's input (poll, a non-blocking
// stdin, tcflush), and none is allowed by lint: golang.org/x/term has neither,
// the syscall allowlist has none of them, and /dev/tty opened non-blocking
// relies on kqueue on macOS, which is reported not to support it (unverified).
type Decoder struct {
	paste bool   // inside a bracketed paste
	held  []byte // the start of a sequence, or of a paste's end, not yet whole
}

// The bracketed paste markers.
var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// maxSeq bounds an escape sequence held across reads: a longer one is no key.
const maxSeq = 32

const esc = 0x1b

// Feed takes one read and returns the keys it answers with.
func (d *Decoder) Feed(b []byte) []Input {
	if len(d.held) > 0 {
		b = append(d.held, b...)
		d.held = nil
	}
	var keys []Input
	bad, pasted, partial := false, false, false
scan:
	for i := 0; i < len(b); {
		rest := b[i:]
		if d.paste {
			j := bytes.Index(rest, pasteEnd)
			if j < 0 {
				d.held = endPrefix(rest)
				break scan
			}
			d.paste, pasted = false, true
			i += j + len(pasteEnd)
			continue
		}
		c := rest[0]
		if c == esc {
			n, in, kind := escSeq(rest)
			switch kind {
			case seqPartial:
				if len(rest) <= maxSeq {
					d.held = append([]byte(nil), rest...)
				}
				partial = true // what came before it in this read goes with it
				break scan
			case seqPaste:
				d.paste, pasted = true, true
			case seqKey:
				keys = append(keys, in)
			default:
				bad = true
			}
			i += n
			continue
		}
		switch {
		case c == '\r' || c == '\n':
			keys = append(keys, Input{Key: KeyEnter})
		case c == 0x03:
			keys = append(keys, Input{Key: KeyInterrupt})
		case c == 0x10: // Ctrl-P
			keys = append(keys, Input{Key: KeyUp})
		case c == 0x0e: // Ctrl-N
			keys = append(keys, Input{Key: KeyDown})
		case c == 0x7f || c == 0x08:
			keys = append(keys, Input{Key: KeyBack})
		case c >= 0x20 && c < 0x7f:
			keys = append(keys, Input{Key: KeyRune, Rune: rune(c)})
		default:
			bad = true
		}
		i++
	}
	// Ctrl-C always ends the list, whatever came with it (PR #140 review).
	if slices.Contains(keys, Input{Key: KeyInterrupt}) {
		return []Input{{Key: KeyInterrupt}}
	}
	if bad || pasted {
		return unknown
	}
	if partial || len(keys) == 0 {
		return nil
	}
	for _, k := range keys[1:] {
		if k != keys[0] || k.Key != KeyUp && k.Key != KeyDown {
			return unknown
		}
	}
	return keys
}

// unknown is what a read that answers nothing returns: one KeyUnknown, which
// the select takes as typing.
var unknown = []Input{{Key: KeyUnknown}}

// endPrefix is the end of b that could be the start of a paste's end marker,
// held for the next read.
func endPrefix(b []byte) []byte {
	for n := min(len(b), len(pasteEnd)-1); n > 0; n-- {
		if bytes.HasPrefix(pasteEnd, b[len(b)-n:]) {
			return append([]byte(nil), b[len(b)-n:]...)
		}
	}
	return nil
}

// seqKind is what escSeq found.
type seqKind int

const (
	seqUnknown seqKind = iota
	seqKey
	seqPaste
	seqPartial
)

// escSeq reads the escape sequence at the start of b, which starts with ESC:
// its length, the key it is, and what kind of sequence it is.
func escSeq(b []byte) (int, Input, seqKind) {
	if len(b) == 1 {
		return 1, Input{Key: KeyEsc}, seqKey
	}
	switch b[1] {
	case '[':
		i := 2
		for i < len(b) && b[i] >= 0x20 && b[i] <= 0x3f {
			i++
		}
		if i == len(b) {
			return 0, Input{}, seqPartial
		}
		if b[i] < 0x40 || b[i] > 0x7e {
			return i, Input{}, seqUnknown
		}
		seq := b[:i+1]
		switch {
		case bytes.Equal(seq, []byte("\x1b[A")):
			return i + 1, Input{Key: KeyUp}, seqKey
		case bytes.Equal(seq, []byte("\x1b[B")):
			return i + 1, Input{Key: KeyDown}, seqKey
		case bytes.Equal(seq, pasteStart):
			return i + 1, Input{}, seqPaste
		}
		return i + 1, Input{}, seqUnknown
	case 'O':
		if len(b) < 3 {
			return 0, Input{}, seqPartial
		}
		switch b[2] {
		case 'A':
			return 3, Input{Key: KeyUp}, seqKey
		case 'B':
			return 3, Input{Key: KeyDown}, seqKey
		}
		return 3, Input{}, seqUnknown
	}
	return 2, Input{}, seqUnknown
}
