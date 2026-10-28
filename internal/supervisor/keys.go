package supervisor

// The switch key is the only input aggy looks at. It arrives either as its
// legacy control byte (Ctrl+\ is 0x1c) or, once a CLI turns on the kitty
// keyboard protocol, as a CSI-u report (Ctrl+\ is ESC [ 92 ; 5 u). Inside
// a bracketed paste (ESC [ 200 ~ ... ESC [ 201 ~) it is data.
//
// The control byte cannot occur inside a UTF-8 sequence (whose bytes are
// all 0x80 or more) or inside the escape sequences terminals send for
// keys and the mouse, so it needs no parsing beyond the paste state.

// keyEvent is one piece of input: data for the CLI, or a switch-key press
// (data then holds its bytes, for passing it on).
type keyEvent struct {
	press bool
	data  []byte
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

// feed splits b into data and presses. The data slices alias b.
func (f *keyFilter) feed(b []byte) []keyEvent {
	var out []keyEvent
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
			}
		}
		f.track(c)
	}
	flush(len(b))
	return out
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
