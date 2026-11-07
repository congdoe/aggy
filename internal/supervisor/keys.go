package supervisor

import "bytes"

// The switch key is the only input aggy looks at. It arrives either as its
// legacy control byte (Ctrl+\ is 0x1c) or, once a CLI turns on the kitty
// keyboard protocol, as a CSI-u report (Ctrl+\ is ESC [ 92 ; 5 u). Inside
// a bracketed paste (ESC [ 200 ~ ... ESC [ 201 ~) it is data.
//
// The control byte cannot occur inside a UTF-8 sequence (whose bytes are
// all 0x80 or more) or inside the escape sequences terminals send for
// keys and the mouse, so it needs no parsing beyond the paste state.

// keyEvent is one piece of input: data for the CLI, or a switch-key press
// (data then holds its bytes, for passing it on). report: the data is the
// terminal's own reply to the CLI (terminalReport's kinds), which is nobody's
// typing. mouse: it is a click or a drag, which may do something in the CLI
// but types no text.
type keyEvent struct {
	press, report, mouse bool
	data                 []byte
}

const esc = 0x1b

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// keyFilter finds switch-key presses in the input stream. Its state
// survives from one read to the next, so a paste marker split across
// reads is still seen.
type keyFilter struct {
	key     byte // the control byte, e.g. 0x1c
	code    int  // its kitty key code, e.g. 92 for '\'
	inPaste bool
	matched int // bytes of the next paste marker matched so far
	// held is an escape sequence that has begun and has not ended, which may
	// be a terminal's reply or a key's report split across reads. It reaches
	// nobody until it ends or flush gives it up, so a reply's digits never
	// count as typing or pick a vendor. A lone Esc is not held: it has to
	// reach the CLI at once, so a split just after Esc counts as typed, the
	// safe side.
	held []byte
}

// maxHeld bounds a held sequence: one longer goes on as keys.
const maxHeld = 4 << 10

// holding reports whether a sequence is held for the next read.
func (f *keyFilter) holding() bool { return len(f.held) > 0 }

// flush gives up on the held sequence: it is input after all.
func (f *keyFilter) flush() []keyEvent {
	if len(f.held) == 0 {
		return nil
	}
	b := f.held
	f.held = nil
	for _, c := range b {
		f.track(c)
	}
	return []keyEvent{{data: b}}
}

// strip is feed for input read while a prompt of aggy's is up, which takes the
// bytes as they came: the terminal's replies come out whole, for the CLI, and
// rest is everything else, in order. A sequence split across reads is held as
// feed holds it. Inside a paste everything is rest.
func (f *keyFilter) strip(b []byte) (reports, rest []byte) {
	if f.inPaste {
		return nil, b
	}
	if len(f.held) > 0 {
		b = append(f.held, b...)
		f.held = nil
	}
	for i := 0; i < len(b); i++ {
		if b[i] == esc {
			n, k := scanReport(b[i:])
			switch k {
			case isReport:
				reports = append(reports, b[i:i+n]...)
				i += n - 1
				continue
			case mayBeReport:
				f.held = append([]byte(nil), b[i:]...)
				return reports, rest
			}
		}
		rest = append(rest, b[i])
	}
	return reports, rest
}

func newKeyFilter(key byte) *keyFilter {
	return &keyFilter{key: key, code: kittyCode(key)}
}

// kittyCode is the key code the kitty protocol reports for Ctrl plus the
// key whose control byte is b: the lowercase letter, or \ ] ^ _.
func kittyCode(b byte) int {
	if b >= 1 && b <= 26 {
		return int('a' + b - 1)
	}
	return int(b | 0x40) // 0x1c '\', 0x1d ']', 0x1e '^', 0x1f '_'
}

// feed splits b into data, the terminal's replies, mouse reports, and presses.
// The data slices alias b, or a copy of it joined to the held sequence.
func (f *keyFilter) feed(b []byte) []keyEvent {
	var out []keyEvent
	if len(f.held) > 0 {
		b = append(f.held, b...)
		f.held = nil
	}
	start := 0
	flush := func(end int) {
		if end > start {
			out = append(out, keyEvent{data: b[start:end]})
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !f.inPaste {
			if c == f.key {
				flush(i)
				out = append(out, keyEvent{press: true, data: b[i : i+1]})
				start = i + 1
				f.matched = 0
				continue
			}
			// A kitty report is matched only when it is whole in this
			// read; terminals write each key's report in one write.
			if c == esc {
				if r, ok := parseCSIu(b[i:]); ok && f.isPress(r) {
					flush(i)
					out = append(out, keyEvent{press: true, data: b[i : i+r.n]})
					i += r.n - 1
					start = i + 1
					f.matched = 0
					continue
				}
				n, k := scanReport(b[i:])
				switch k {
				case isReport, isMouse:
					flush(i)
					out = append(out, keyEvent{report: k == isReport, mouse: k == isMouse, data: b[i : i+n]})
					i += n - 1
					start = i + 1
					f.matched = 0
					continue
				case mayBeReport:
					flush(i)
					f.held = append([]byte(nil), b[i:]...)
					return out
				}
			}
		}
		f.track(c)
	}
	flush(len(b))
	return out
}

// reportKind is what scanReport found.
type reportKind int

const (
	notReport   reportKind = iota // a key, as far as aggy can say
	isReport                      // the terminal's own reply, or the wheel, or a hover
	isMouse                       // a click or a drag
	mayBeReport                   // it could still become one: more bytes are needed
)

// scanReport reads the escape sequence at the start of b, which starts with
// ESC, and says what it is and how long. These are replies a terminal sends a
// CLI that asked, none of which can submit anything: focus events, CPR, DA1 and
// DA2, DSR, window reports, DECRPM, the kitty keyboard query's answer, whole
// OSC, DCS, and APC strings (OSC 11, XTVERSION, XTGETTCAP), and the mouse
// wheel and a hover, in SGR or X10 encoding. A key's sequence, a click, and a
// paste marker are not; a click or a drag can do something, as far as aggy
// knows, so it is isMouse. ESC alone is notReport: see keyFilter.held.
func scanReport(b []byte) (int, reportKind) {
	if len(b) < 2 || len(b) > maxHeld {
		return 0, notReport
	}
	switch b[1] {
	case '[':
		return scanCSI(b)
	case ']', 'P', '_':
		return scanString(b)
	}
	return 0, notReport
}

// scanCSI is scanReport for ESC [.
func scanCSI(b []byte) (int, reportKind) {
	if len(b) == 2 {
		return 0, mayBeReport
	}
	switch b[2] {
	case 'M':
		if len(b) < 6 {
			return 0, mayBeReport
		}
		// X10: three bytes, each its value plus 32.
		if b[3] < 32 || b[4] < 32 || b[5] < 32 {
			return 0, notReport
		}
		return 6, mouseKind(int(b[3]) - 32)
	case '<':
		return scanSGRMouse(b)
	}
	i, colon := 2, false
	for i < len(b) && (b[i] >= '0' && b[i] <= '9' || b[i] == ';' || b[i] == '?' || b[i] == '>' || b[i] == ':') {
		colon = colon || b[i] == ':'
		i++
	}
	if i == len(b) {
		return 0, mayBeReport // also a kitty key's report, split
	}
	if colon {
		return 0, notReport
	}
	switch b[i] {
	case 'I', 'O':
		if i == 2 {
			return 3, isReport
		}
	case 'R', 'c', 'n', 't':
		return i + 1, isReport
	case '$':
		if i+1 == len(b) {
			return 0, mayBeReport
		}
		if b[i+1] == 'y' && !bytes.ContainsRune(b[2:i], '>') {
			return i + 2, isReport
		}
	case 'u':
		if b[2] == '?' && !bytes.ContainsAny(b[3:i], ";?>") {
			return i + 1, isReport
		}
	}
	return 0, notReport
}

// scanSGRMouse is scanCSI for ESC [ <: button ; column ; row, then M or m.
func scanSGRMouse(b []byte) (int, reportKind) {
	i, field, button, digits := 3, 0, 0, 0
	for ; i < len(b); i++ {
		switch c := b[i]; {
		case c >= '0' && c <= '9':
			if digits++; digits > 7 {
				return 0, notReport
			}
			if field == 0 {
				button = button*10 + int(c-'0')
			}
		case c == ';' && digits > 0 && field < 2:
			field, digits = field+1, 0
		case (c == 'M' || c == 'm') && digits > 0 && field == 2:
			return i + 1, mouseKind(button)
		default:
			return 0, notReport
		}
	}
	return 0, mayBeReport
}

// mouseKind sorts a mouse report by its button code: the wheel (bit 64, bits
// up to 16 being modifiers) and a move with no button held (bit 32, low bits
// 3) cannot submit anything. xterm's ctlseqs give the codes.
func mouseKind(button int) reportKind {
	switch {
	case button >= 64 && button <= 95, button >= 32 && button <= 63 && button&3 == 3:
		return isReport
	}
	return isMouse
}

// scanString is scanReport for a whole OSC, DCS, or APC string, ended by BEL
// or ST. Any other control character means it was never one: Alt+P and text.
func scanString(b []byte) (int, reportKind) {
	for i := 2; i < len(b); i++ {
		switch c := b[i]; {
		case c == 0x07:
			return i + 1, isReport
		case c == esc:
			if i+1 == len(b) {
				return 0, mayBeReport
			}
			if b[i+1] == '\\' {
				return i + 2, isReport
			}
			return 0, notReport
		case c < 0x20:
			return 0, notReport
		}
	}
	return 0, mayBeReport
}

// terminalReport reports whether b is only such replies, whole.
func terminalReport(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for len(b) > 0 {
		if b[0] != esc {
			return false
		}
		n, k := scanReport(b)
		if k != isReport {
			return false
		}
		b = b[n:]
	}
	return true
}

// mouseOnly reports whether b is one click or drag, whole.
func mouseOnly(b []byte) bool {
	if len(b) == 0 || b[0] != esc {
		return false
	}
	n, k := scanReport(b)
	return k == isMouse && n == len(b)
}

// track advances the paste-marker match by one byte. The markers start
// with ESC and hold no other ESC, so a mismatch restarts at this byte.
func (f *keyFilter) track(c byte) {
	m := pasteStart
	if f.inPaste {
		m = pasteEnd
	}
	switch c {
	case m[f.matched]:
		f.matched++
		if f.matched == len(m) {
			f.inPaste = !f.inPaste
			f.matched = 0
		}
	case m[0]:
		f.matched = 1
	default:
		f.matched = 0
	}
}

// isPress reports whether r is a press (or an unreported event type) of
// Ctrl plus the switch key, with no other modifier than Caps or Num Lock.
func (f *keyFilter) isPress(r csiU) bool {
	const ctrl, locks = 4, 64 | 128
	return r.code == f.code && (r.mods-1)&^locks == ctrl && (r.event == 0 || r.event == 1)
}

// csiU is a parsed kitty keyboard report, ESC [ code[:alt...] [; mods[:event]
// [; text]] u, or an xterm modifyOtherKeys report, ESC [ 27 ; mods ; code ~,
// which carries the same code and modifiers.
type csiU struct {
	code, mods, event int
	n                 int // its length in bytes
}

// parseCSIu parses a kitty or modifyOtherKeys report at the start of b.
// TODO(T6684): the terminal checklist records whether any CLI turns on
// modifyOtherKeys or kitty's report-text flag (16); both are parsed.
func parseCSIu(b []byte) (csiU, bool) {
	r := csiU{mods: 1}
	if len(b) < 4 || b[0] != esc || b[1] != '[' {
		return r, false
	}
	if m, ok := parseModifyOtherKeys(b); ok {
		return m, true
	}
	i := 2
	num := func() (int, bool) {
		v, d := 0, 0
		for i < len(b) && b[i] >= '0' && b[i] <= '9' && d < 7 {
			v = v*10 + int(b[i]-'0')
			i++
			d++
		}
		return v, d > 0
	}
	var ok bool
	if r.code, ok = num(); !ok {
		return r, false
	}
	for i < len(b) && b[i] == ':' { // alternate key codes
		i++
		num()
	}
	if i < len(b) && b[i] == ';' {
		i++
		if r.mods, ok = num(); !ok {
			r.mods = 1
		}
		if i < len(b) && b[i] == ':' {
			i++
			if r.event, ok = num(); !ok {
				return r, false
			}
		}
		// Kitty's report-text flag adds the text as code points.
		if i < len(b) && b[i] == ';' {
			i++
			for i < len(b) && (b[i] >= '0' && b[i] <= '9' || b[i] == ':') && i < 64 {
				i++
			}
		}
	}
	if i >= len(b) || b[i] != 'u' {
		return r, false
	}
	r.n = i + 1
	return r, true
}

// parseModifyOtherKeys parses xterm's ESC [ 27 ; mods ; code ~ at the
// start of b.
func parseModifyOtherKeys(b []byte) (csiU, bool) {
	const prefix = "\x1b[27;"
	if len(b) < len(prefix) || string(b[:len(prefix)]) != prefix {
		return csiU{}, false
	}
	i := len(prefix)
	var f [2]int
	for k := range f {
		d := 0
		for i < len(b) && b[i] >= '0' && b[i] <= '9' && d < 7 {
			f[k] = f[k]*10 + int(b[i]-'0')
			i++
			d++
		}
		want := byte(';')
		if k == 1 {
			want = '~'
		}
		if d == 0 || i >= len(b) || b[i] != want {
			return csiU{}, false
		}
		i++
	}
	return csiU{code: f[1], mods: f[0], n: i}, true
}
