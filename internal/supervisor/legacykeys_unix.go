//go:build unix

package supervisor

import (
	"bytes"
	"unicode/utf8"
)

// kitty's modifier bits, once 1 is taken off its field.
const (
	modShift = 1
	modCtrl  = 4
	modLocks = 64 | 128
)

// kittyPrivate starts the key codes kitty keeps for functional keys, up to
// kittyPrivateEnd; keypad Enter is one of them, 57414.
const (
	kittyPrivate    = 57344
	kittyPrivateEnd = 63743
	kittyKeypadCR   = 57414
)

// legacyKeys rewrites a read's kitty and modifyOtherKeys reports as the bytes a
// legacy terminal sends for the same key, for the composer tracking only: a CLI in
// kitty's report-all mode must not type text aggy cannot see, or an Enter it cannot
// count. A press or repeat of a printable key with no Ctrl or Alt is that character;
// Enter, Backspace, Tab, and Shift+Tab are their legacy forms, and Ctrl plus a letter
// its control byte. A release is dropped. Anything else stays as it is, for the
// tracking to treat as a key it cannot judge. Inside a paste nothing changes; inPaste
// is the state the read starts in.
func legacyKeys(b []byte, inPaste bool) []byte {
	if bytes.IndexByte(b, esc) < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		rest := b[i:]
		switch {
		case bytes.HasPrefix(rest, pasteStart):
			inPaste = true
		case bytes.HasPrefix(rest, pasteEnd):
			inPaste = false
		}
		if !inPaste && rest[0] == esc {
			if r, ok := parseCSIu(rest); ok {
				if r.event != 3 {
					out = append(out, legacyKey(r, rest[:r.n])...)
				}
				i += r.n
				continue
			}
		}
		out = append(out, rest[0])
		i++
	}
	return out
}

// legacyKey is legacyKeys for one report, whose bytes are raw.
func legacyKey(r csiU, raw []byte) []byte {
	mods := (r.mods - 1) &^ modLocks
	switch {
	case r.code == 9 && mods == modShift:
		return []byte("\x1b[Z")
	case mods == modCtrl && r.code >= 'a' && r.code <= 'z':
		return []byte{byte(r.code - 'a' + 1)}
	case mods == 0 && (r.code == 13 || r.code == kittyKeypadCR):
		return []byte{'\r'}
	case mods == 0 && (r.code == 127 || r.code == 8):
		return []byte{0x7f}
	case mods == 0 && r.code == 9:
		return []byte{'\t'}
	case mods&^modShift != 0, r.code < 0x20, r.code == 127, r.code >= kittyPrivate && r.code <= kittyPrivateEnd, !utf8.ValidRune(rune(r.code)):
		return raw
	}
	return utf8.AppendRune(nil, rune(r.code))
}
