// Package termsafe escapes untrusted text before it reaches a terminal.
// Vendor sessions and other tools' config files can hold escape sequences,
// bidi overrides, and other bytes a terminal acts on rather than prints, so
// aggy log and the --dry-run previews pass their text through here.
package termsafe

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Field escapes s for one cell of a table: every C0 control (including
// tab, which text/tabwriter would take as a column break, CR, and
// newline), DEL, C1 controls, Unicode format characters such as bidi
// overrides, and invalid UTF-8. Newline, CR, and tab become \n, \r, and
// \t; other bytes become \xNN or \uNNNN.
func Field(s string) string { return escape(s, false) }

// Line escapes s like Field but keeps tabs, for a line of file content
// shown as is, such as a line of a diff. s must not hold the line's
// newline; a newline inside s is escaped.
func Line(s string) string { return escape(s, true) }

// Lines escapes every line of s with Line and keeps the newlines between
// them, so multi-line text keeps its shape but cannot move the cursor or
// change the terminal's state.
func Lines(s string) string {
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = Line(p)
	}
	return strings.Join(parts, "\n")
}

func escape(s string, keepTab bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case r == '\t' && keepTab:
			b.WriteByte('\t')
		case r == '\t':
			b.WriteString("\\t")
		case r == '\n':
			b.WriteString("\\n")
		case r == '\r':
			b.WriteString("\\r")
		case r < 0x20, r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r <= 0x9f, unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}
