package trustlist

import (
	"slices"
	"strings"
	"testing"
)

// TestDecoder (T6981): a terminal's reads as keys at the select: every key,
// sequences split across reads, sequences aggy does not know, pastes, and
// bursts.
func TestDecoder(t *testing.T) {
	u := []Input{unkn} // a read that is no key: typing
	cases := []struct {
		name  string
		reads []string
		want  [][]Input // the keys of each read
	}{
		{"Up", []string{"\x1b[A"}, [][]Input{{up}}},
		{"Down", []string{"\x1b[B"}, [][]Input{{down}}},
		{"Up, application mode", []string{"\x1bOA"}, [][]Input{{up}}},
		{"Down, application mode", []string{"\x1bOB"}, [][]Input{{down}}},
		{"Ctrl-P", []string{"\x10"}, [][]Input{{up}}},
		{"Ctrl-N", []string{"\x0e"}, [][]Input{{down}}},
		{"k and j", []string{"k", "j"}, [][]Input{{rn('k')}, {rn('j')}}},
		{"Space", []string{" "}, [][]Input{{rn(' ')}}},
		{"Enter", []string{"\r"}, [][]Input{{enter}}},
		{"Enter as a newline", []string{"\n"}, [][]Input{{enter}}},
		{"Esc", []string{"\x1b"}, [][]Input{{escK}}},
		{"Ctrl-C", []string{"\x03"}, [][]Input{{ctrlC}}},
		{"Backspace", []string{"\x7f", "\x08"}, [][]Input{{back}, {back}}},
		{"n, x, a digit", []string{"n", "x", "7"}, [][]Input{{rn('n')}, {rn('x')}, {rn('7')}}},
		{"a held arrow", []string{"\x1b[B\x1b[B\x1b[B"}, [][]Input{{down, down, down}}},

		{"split after the bracket", []string{"\x1b[", "A"}, [][]Input{nil, {up}}},
		{"split in application mode", []string{"\x1bO", "B"}, [][]Input{nil, {down}}},
		{"split long sequence", []string{"\x1b[1", ";5", "A"}, [][]Input{nil, nil, u}},
		{"a key before a split sequence goes with it", []string{"j\x1b[", "B"}, [][]Input{nil, {down}}},
		// PR #140 review, L2: Ctrl-C after a sequence cut off still ends it.
		{"Ctrl-C after a split sequence", []string{"\x1b[", "\x03"}, [][]Input{nil, {ctrlC}}},
		{"Ctrl-C in a burst", []string{"ab\x03"}, [][]Input{{ctrlC}}},
		{"Ctrl-C before a split sequence", []string{"\x03\x1b["}, [][]Input{{ctrlC}}},
		{"Ctrl-C in the read a paste starts", []string{"\x03\x1b[200~x"}, [][]Input{{ctrlC}}},
		{"Ctrl-C inside a paste is pasted", []string{"\x1b[200~x", "\x03", "\x1b[201~"}, [][]Input{u, nil, u}},

		{"Alt-x is not Esc then x", []string{"\x1bx"}, [][]Input{u}},
		{"an unknown CSI is not Esc then letters", []string{"\x1b[1;5A"}, [][]Input{u}},
		{"F1 is not Esc then O and P", []string{"\x1bOP"}, [][]Input{u}},
		{"a kitty key report", []string{"\x1b[120u"}, [][]Input{u}},
		{"a mouse report", []string{"\x1b[<0;10;5M"}, [][]Input{u}},
		{"focus in", []string{"\x1b[I"}, [][]Input{u}},
		{"an unknown sequence then x in one read", []string{"\x1b[Zx"}, [][]Input{u}},
		{"an unknown sequence then a key alone", []string{"\x1b[Z", "x"}, [][]Input{u, {rn('x')}}},
		{"Tab", []string{"\t"}, [][]Input{u}},
		{"UTF-8", []string{"é"}, [][]Input{u}},
		{"a sequence too long to hold", []string{"\x1b[" + strings.Repeat("1", 40), "A"}, [][]Input{nil, {rn('A')}}},

		{"a burst of typing", []string{"2\r"}, [][]Input{u}},
		{"a burst of letters", []string{"xx"}, [][]Input{u}},
		{"Esc after a key", []string{"j\x1b"}, [][]Input{u}},
		{"arrows mixed", []string{"\x1b[A\x1b[B"}, [][]Input{u}},

		{"a paste", []string{"\x1b[200~2\rx\x1b[201~"}, [][]Input{u}},
		{"a paste then a key", []string{"\x1b[200~x\x1b[201~", "\r"}, [][]Input{u, {enter}}},
		{"a paste across reads", []string{"\x1b[200~2", "\r", "x\x1b[201~", "n"}, [][]Input{u, nil, u, {rn('n')}}},
		{"a paste's end split", []string{"\x1b[200~x\x1b[2", "01~", "\r"}, [][]Input{u, u, {enter}}},
		{"a paste's start split", []string{"\x1b[20", "0~\r\x1b[201~", "x"}, [][]Input{nil, u, {rn('x')}}},
		{"a key in the read a paste starts", []string{"j\x1b[200~x"}, [][]Input{u}},
		{"a stray paste end", []string{"\x1b[201~"}, [][]Input{u}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var d Decoder
			for i, r := range c.reads {
				if got := d.Feed([]byte(r)); !slices.Equal(got, c.want[i]) {
					t.Errorf("read %d %q: keys %v, want %v", i, r, got, c.want[i])
				}
			}
		})
	}
}
