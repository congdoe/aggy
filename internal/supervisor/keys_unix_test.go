//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestPickerOptions (T6665): the toggles, the model edit, and the child
// list of a vendor with several children in the parent.
func TestPickerOptions(t *testing.T) {
	two := []adapter.Vendor{adapter.Codex, adapter.Grok}
	kids := func(v adapter.Vendor) []ChildChoice {
		if v == adapter.Grok {
			return []ChildChoice{{"g2", "latest"}, {"g1", "older"}}
		}
		return []ChildChoice{{"c1", "only"}}
	}
	tests := []struct {
		name string
		in   []string
		done bool
		want Choice
	}{
		{"one child resumes the latest", []string{"1"}, true, Choice{Vendor: adapter.Codex}},
		{"new and brief", []string{"n", "b", "1"}, true, Choice{Vendor: adapter.Codex, New: true, Brief: true}},
		{"several children ask which", []string{"2"}, false, Choice{}},
		{"Enter takes the latest", []string{"2", "\r"}, true, Choice{Vendor: adapter.Grok, Child: "g2"}},
		{"a digit takes that child", []string{"2", "2"}, true, Choice{Vendor: adapter.Grok, Child: "g1"}},
		{"n in the child list starts one", []string{"2", "n"}, true, Choice{Vendor: adapter.Grok, New: true}},
		{"Esc in the child list goes back", []string{"2", "\x1b", "1"}, true, Choice{Vendor: adapter.Codex}},
		{"new skips the child list", []string{"n", "2"}, true, Choice{Vendor: adapter.Grok, New: true}},
		{"a model", []string{"m", "gpt-5.5", "\x7f5", "\r", "1"}, true, Choice{Vendor: adapter.Codex, Model: "gpt-5.5"}},
		{"a bad model is not taken", []string{"m", "a b;c", "\r", "1"}, false, Choice{}},
		{"Esc drops the model edit", []string{"m", "x", "\x1b", "1"}, true, Choice{Vendor: adapter.Codex}},
		{"a preselect is what Enter picks", []string{"P", "\r", "\r"}, true, Choice{Vendor: adapter.Grok, Child: "g2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &picker{vendors: two, key: 0x1c, children: kids}
			var done bool
			var c Choice
			for _, in := range tt.in {
				if in == "P" {
					p.preselect = adapter.Grok
					continue
				}
				done, c = p.handle([]byte(in))
			}
			if done != tt.done || c != tt.want {
				t.Errorf("got %v %+v, want %v %+v", done, c, tt.done, tt.want)
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
		{"toggles do not pick", two, "nbn", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &picker{vendors: tt.vendors, key: 0x1c}
			done, c := p.handle([]byte(tt.in))
			if picked := c.Vendor; done != tt.done || picked != tt.picked {
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
