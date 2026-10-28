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
		if e.press {
			fmt.Fprintf(&b, "<P:%q>", e.data)
		} else {
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
			`"\x1b[20""0~x\x1c""y\x1b[2""01~"<P:"\x1c">`},
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
		{"mouse and osc untouched", 0x1c, []string{"\x1b[<0;10;5M\x1b]52;c;eA==\x07"}, `"\x1b[<0;10;5M\x1b]52;c;eA==\a"`},
		{"truncated kitty is data", 0x1c, []string{"\x1b[92;5"}, `"\x1b[92;5"`},
		// B13-8: kitty's report-text field and xterm modifyOtherKeys.
		{"kitty press with text", 0x1c, []string{"\x1b[92;5;92u"}, `<P:"\x1b[92;5;92u">`},
		{"kitty press with event and text", 0x1c, []string{"\x1b[92;5:1;92:93u"}, `<P:"\x1b[92;5:1;92:93u">`},
		{"modifyOtherKeys press", 0x1c, []string{"a\x1b[27;5;92~b"}, `"a"<P:"\x1b[27;5;92~">"b"`},
		{"modifyOtherKeys other key is data", 0x1c, []string{"\x1b[27;5;97~"}, `"\x1b[27;5;97~"`},
		{"modifyOtherKeys ctrl+shift is data", 0x1c, []string{"\x1b[27;6;92~"}, `"\x1b[27;6;92~"`},
		{"truncated modifyOtherKeys is data", 0x1c, []string{"\x1b[27;5;92"}, `"\x1b[27;5;92"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newKeyFilter(tt.key)
			var got string
			for _, c := range tt.chunks {
				got += render(f.feed([]byte(c)))
			}
			if got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}
