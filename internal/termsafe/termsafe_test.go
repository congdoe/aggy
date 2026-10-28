package termsafe

import "testing"

func TestEscape(t *testing.T) {
	tests := []struct {
		name, in, field, line string
	}{
		{"plain", "codex (gpt) on screen", "codex (gpt) on screen", "codex (gpt) on screen"},
		{"tab", "a\tb", `a\tb`, "a\tb"},
		{"newline and CR", "a\r\nb", `a\r\nb`, `a\r\nb`},
		{"escape sequence", "\x1b[31mred", `\x1b[31mred`, `\x1b[31mred`},
		{"DEL", "a\x7fb", `a\x7fb`, `a\x7fb`},
		{"C1", "a\u009bb", `a\u009bb`, `a\u009bb`},
		{"bidi override", "a\u202eb", "a\\u202eb", "a\\u202eb"},
		{"invalid UTF-8", "a\xffb", `a\xffb`, `a\xffb`},
		{"unicode text", "José ß", "José ß", "José ß"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Field(tt.in); got != tt.field {
				t.Errorf("Field(%q) = %q, want %q", tt.in, got, tt.field)
			}
			if got := Line(tt.in); got != tt.line {
				t.Errorf("Line(%q) = %q, want %q", tt.in, got, tt.line)
			}
		})
	}
}

func TestLines(t *testing.T) {
	in := "--- a\n+x\x1b]0;t\x07\r\n\tkeep\n"
	want := "--- a\n+x\\x1b]0;t\\x07\\r\n\tkeep\n"
	if got := Lines(in); got != want {
		t.Errorf("Lines = %q, want %q", got, want)
	}
}
