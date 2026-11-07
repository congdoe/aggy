package supervisor

import (
	"bytes"
	"fmt"
	"testing"
)

// render shows events as text: data as %q, a press as <P:%q>.
func render(evs []keyEvent) string {
	var b bytes.Buffer
	for _, e := range evs {
		switch {
		case e.press:
			fmt.Fprintf(&b, "<P:%q>", e.data)
		case e.report:
			fmt.Fprintf(&b, "<R:%q>", e.data)
		case e.mouse:
			fmt.Fprintf(&b, "<M:%q>", e.data)
		default:
			fmt.Fprintf(&b, "%q", e.data)
		}
	}
	return b.String()
}

func TestKeyFilter(t *testing.T) {
	tests := []struct {
		name   string
		key    byte
		chunks []string
		want   string // the rendered events of every chunk, joined
	}{
		{"plain text", 0x1c, []string{"hello"}, `"hello"`},
		{"legacy press", 0x1c, []string{"a\x1cb"}, `"a"<P:"\x1c">"b"`},
		{"two presses", 0x1c, []string{"\x1c\x1c"}, `<P:"\x1c"><P:"\x1c">`},
		{"other ctrl byte is data", 0x1c, []string{"\x1d\x03"}, `"\x1d\x03"`},
		{"configured key", 0x1d, []string{"\x1c\x1d"}, `"\x1c"<P:"\x1d">`},
		{"key inside paste is data", 0x1c, []string{"\x1b[200~a\x1cb\x1b[201~\x1c"},
			`"\x1b[200~a\x1cb\x1b[201~"<P:"\x1c">`},
		{"paste start split across reads", 0x1c, []string{"\x1b[20", "0~x\x1c", "y\x1b[2", "01~\x1c"},
			`"\x1b[200~x\x1c""y\x1b[2""01~"<P:"\x1c">`},
		{"paste end split byte by byte", 0x1c, []string{"\x1b[200~", "\x1b", "[", "2", "0", "1", "~", "\x1c"},
			`"\x1b[200~""\x1b""[""2""0""1""~"<P:"\x1c">`},
		{"almost a paste end stays in paste", 0x1c, []string{"\x1b[200~\x1b[202~\x1c\x1b[201~"},
			`"\x1b[200~\x1b[202~\x1c\x1b[201~"`},
		{"kitty press", 0x1c, []string{"a\x1b[92;5ub"}, `"a"<P:"\x1b[92;5u">"b"`},
		{"kitty press event type", 0x1c, []string{"\x1b[92;5:1u"}, `<P:"\x1b[92;5:1u">`},
		{"kitty press with caps lock", 0x1c, []string{"\x1b[92;69u"}, `<P:"\x1b[92;69u">`},
		{"kitty release is data", 0x1c, []string{"\x1b[92;5:3u"}, `"\x1b[92;5:3u"`},
		{"kitty repeat is data", 0x1c, []string{"\x1b[92;5:2u"}, `"\x1b[92;5:2u"`},
		{"kitty without ctrl is data", 0x1c, []string{"\x1b[92u\x1b[92;1u"}, `"\x1b[92u\x1b[92;1u"`},
		{"kitty ctrl+shift is data", 0x1c, []string{"\x1b[92;6u"}, `"\x1b[92;6u"`},
		{"kitty other key is data", 0x1c, []string{"\x1b[97;5u"}, `"\x1b[97;5u"`},
		{"kitty in paste is data", 0x1c, []string{"\x1b[200~\x1b[92;5u\x1b[201~"}, `"\x1b[200~\x1b[92;5u\x1b[201~"`},
		{"kitty ctrl+a for ctrl+a key", 0x01, []string{"\x1b[97;5u"}, `<P:"\x1b[97;5u">`},
		{"kitty alternate codes", 0x1c, []string{"\x1b[92:124;5u"}, `<P:"\x1b[92:124;5u">`},
		{"utf-8 around the key", 0x1c, []string{"漢\x1c🙂"}, `"漢"<P:"\x1c">"🙂"`},
		{"mouse and osc untouched", 0x1c, []string{"\x1b[<0;10;5M\x1b]52;c;eA==\x07"}, `<M:"\x1b[<0;10;5M"><R:"\x1b]52;c;eA==\a">`},
		{"truncated kitty is data", 0x1c, []string{"\x1b[92;5"}, `"\x1b[92;5"`},
		// B13-8: kitty's report-text field and xterm modifyOtherKeys.
		{"kitty press with text", 0x1c, []string{"\x1b[92;5;92u"}, `<P:"\x1b[92;5;92u">`},
		{"kitty press with event and text", 0x1c, []string{"\x1b[92;5:1;92:93u"}, `<P:"\x1b[92;5:1;92:93u">`},
		{"modifyOtherKeys press", 0x1c, []string{"a\x1b[27;5;92~b"}, `"a"<P:"\x1b[27;5;92~">"b"`},
		{"modifyOtherKeys other key is data", 0x1c, []string{"\x1b[27;5;97~"}, `"\x1b[27;5;97~"`},
		{"modifyOtherKeys ctrl+shift is data", 0x1c, []string{"\x1b[27;6;92~"}, `"\x1b[27;6;92~"`},
		// T6897: the terminal's replies come apart from the keys around them,
		// and a split one or a split kitty press waits for its rest.
		{"reply between keys", 0x1c, []string{"a\x1b[Ib"}, `"a"<R:"\x1b[I">"b"`},
		{"focus out and in", 0x1c, []string{"\x1b[O\x1b[I"}, `<R:"\x1b[O"><R:"\x1b[I">`},
		{"reply before the key", 0x1c, []string{"\x1b[?62;22c\x1c"}, `<R:"\x1b[?62;22c"><P:"\x1c">`},
		{"reply split across reads", 0x1c, []string{"\x1b[?6", "2;22c"}, `<R:"\x1b[?62;22c">`},
		{"osc split, key after", 0x1c, []string{"\x1b]11;rgb:00", "00/00\x1b", "\\x"}, `<R:"\x1b]11;rgb:0000/00\x1b\\">"x"`},
		{"split kitty press", 0x1c, []string{"\x1b[92;", "5u"}, `<P:"\x1b[92;5u">`},
		{"split key goes on whole", 0x1c, []string{"\x1b[", "A"}, `"\x1b[A"`},
		{"never finished is data", 0x1c, []string{"\x1b[?6"}, `"\x1b[?6"`},
		{"reply inside a paste is data", 0x1c, []string{"\x1b[200~\x1b[I\x1b[201~"}, `"\x1b[200~\x1b[I\x1b[201~"`},
		{"x10 wheel", 0x1c, []string{"\x1b[M\x60!!"}, `<R:"\x1b[M` + "`" + `!!">`},
		{"x10 click", 0x1c, []string{"\x1b[M !!"}, `<M:"\x1b[M !!">`},
		{"x10 split", 0x1c, []string{"\x1b[M ", "!!x"}, `<M:"\x1b[M !!">"x"`},
		{"sgr hover", 0x1c, []string{"\x1b[<35;1;2M"}, `<R:"\x1b[<35;1;2M">`},
		{"sgr drag", 0x1c, []string{"\x1b[<32;1;2M"}, `<M:"\x1b[<32;1;2M">`},
		{"alt+p then a control byte is data", 0x1c, []string{"\x1bPa\r"}, `"\x1bPa\r"`},
		{"truncated modifyOtherKeys is data", 0x1c, []string{"\x1b[27;5;92"}, `"\x1b[27;5;92"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newKeyFilter(tt.key)
			var got string
			for _, c := range tt.chunks {
				got += render(f.feed([]byte(c)))
			}
			got += render(f.flush())
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}
