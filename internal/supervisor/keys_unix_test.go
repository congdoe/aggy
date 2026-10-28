//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

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
