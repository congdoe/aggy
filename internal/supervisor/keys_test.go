package supervisor

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
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

func TestPickerHandle(t *testing.T) {
	two := []adapter.Vendor{adapter.Codex, adapter.Grok}
	tests := []struct {
		name    string
		vendors []adapter.Vendor
		in      string
		done    bool
		picked  adapter.Vendor
	}{
		{"esc cancels", two, "\x1b", true, ""},
		{"kitty esc cancels", two, "\x1b[27u", true, ""},
		{"ctrl+c cancels", two, "\x03", true, ""},
		{"switch key cancels", two, "\x1c", true, ""},
		{"kitty switch key cancels", two, "\x1b[92;5u", true, ""},
		{"enter picks the first", two, "\r", true, adapter.Codex},
		{"kitty enter picks the first", two, "\x1b[13u", true, adapter.Codex},
		{"digit picks", two, "2", true, adapter.Grok},
		{"kitty digit picks", two, "\x1b[50u", true, adapter.Grok},
		{"digit out of range ignored", two, "3", false, ""},
		{"arrow key ignored", two, "\x1b[A", false, ""},
		{"letters ignored", two, "xyz", false, ""},
		{"enter with no vendors ignored", nil, "\r", false, ""},
		{"esc with no vendors cancels", nil, "\x1b", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &picker{vendors: tt.vendors, key: 0x1c}
			done, picked := p.handle([]byte(tt.in))
			if done != tt.done || picked != tt.picked {
				t.Errorf("got %v %q, want %v %q", done, picked, tt.done, tt.picked)
			}
		})
	}
}

func TestPickerDraw(t *testing.T) {
	var b bytes.Buffer
	p := &picker{vendors: []adapter.Vendor{adapter.Grok, adapter.Claude}}
	p.draw(&b)
	p.erase(&b)
	s := b.String()
	for _, want := range []string{altScreenOn, "1  grok", "2  claude", altScreenOff} {
		if !bytes.Contains([]byte(s), []byte(want)) {
			t.Errorf("picker output %q lacks %q", s, want)
		}
	}
	if bytes.Index([]byte(s), []byte("grok")) > bytes.Index([]byte(s), []byte("claude")) {
		t.Error("vendors not in switch.order order")
	}
}
