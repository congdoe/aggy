package supervisor

import (
	"bytes"
	"fmt"
	"strings"
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
	tests := []struct {
		name    string
		overCLI bool
		want    []string
		notWant []string
	}{
		{"own alternate screen", false, []string{altScreenOn, "1  grok", "2  claude", altScreenOff}, []string{"\x1b7"}},
		// B13-13: DECSC/DECRC would overwrite the CLI's own saved cursor.
		{"over the CLI's alternate screen", true, []string{"\x1b[1;1H\x1b[2Kaggy: switch vendor", "1  grok"}, []string{"\x1b[?1049", "\x1b7", "\x1b8"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			p := &picker{vendors: []adapter.Vendor{adapter.Grok, adapter.Claude}}
			p.draw(&b, tt.overCLI)
			p.erase(&b, tt.overCLI)
			s := b.String()
			for _, w := range tt.want {
				if !strings.Contains(s, w) {
					t.Errorf("picker output %q lacks %q", s, w)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(s, w) {
					t.Errorf("picker output %q has %q", s, w)
				}
			}
			if strings.Index(s, "grok") > strings.Index(s, "claude") {
				t.Error("vendors not in switch.order order")
			}
		})
	}
}

func TestAltTracker(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{"nothing", []string{"hello"}, false},
		{"1049 on", []string{"\x1b[?1049h"}, true},
		{"1049 on then off", []string{"\x1b[?1049h..\x1b[?1049l"}, false},
		{"1047 on", []string{"\x1b[?1047h"}, true},
		{"47 on", []string{"\x1b[?47h"}, true},
		{"split across reads", []string{"x\x1b", "[?10", "49", "h"}, true},
		{"several params", []string{"\x1b[?1006;1049h"}, true},
		{"other private mode", []string{"\x1b[?2004h\x1b[?25l"}, false},
		{"1049 as a longer number", []string{"\x1b[?10490h"}, false},
		{"not private", []string{"\x1b[1049h"}, false},
		{"broken then real", []string{"\x1b[?10\x1b[?1049h"}, true},
		{"esc esc", []string{"\x1b\x1b[?1049h"}, true},
		{"text between", []string{"\x1b[?1049h", "screen", "\x1b[?1049l", "more"}, false},
		// B13-12: a full reset (RIS) leaves the alternate screen.
		{"RIS clears", []string{"\x1b[?1049h", "\x1bc"}, false},
		{"RIS split", []string{"\x1b[?1049h\x1b", "c"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var a altTracker
			for _, c := range tt.chunks {
				a.feed([]byte(c))
			}
			if a.on != tt.want {
				t.Errorf("on = %v, want %v", a.on, tt.want)
			}
		})
	}
}
