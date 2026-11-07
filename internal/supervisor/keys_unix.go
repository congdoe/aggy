//go:build unix

package supervisor

// holding reports whether a sequence is held for the next read.
func (f *keyFilter) holding() bool { return len(f.held) > 0 }

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
