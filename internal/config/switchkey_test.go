package config

import (
	"strings"
	"testing"
)

func TestParseKey(t *testing.T) {
	tests := []struct {
		in      string
		want    byte
		wantErr string
	}{
		{`ctrl+\`, 0x1c, ""},
		{`Ctrl+\`, 0x1c, ""},
		{" ctrl+] ", 0x1d, ""},
		{"ctrl+^", 0x1e, ""},
		{"ctrl+_", 0x1f, ""},
		{"ctrl+a", 0x01, ""},
		{"CTRL+G", 0x07, ""},
		{"ctrl+z", 0x1a, ""},
		{"ctrl+i", 0, "Tab or Enter"},
		{"ctrl+j", 0, "Tab or Enter"},
		{"ctrl+m", 0, "Tab or Enter"},
		{"ctrl+[", 0, "want ctrl+"},
		{"ctrl+1", 0, "want ctrl+"},
		{"ctrl+ab", 0, "want ctrl+"},
		{"alt+x", 0, "want ctrl+"},
		{`\`, 0, "want ctrl+"},
		{"", 0, "want ctrl+"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseKey(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got %#x, %v; want %#x", got, err, tt.want)
			}
		})
	}
}

func TestSwitchKeyConfig(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    string
		wantErr string
	}{
		{"default", "", DefaultSwitchKey, ""},
		{"order only keeps the default key", "[switch]\norder = [\"grok\"]\n", DefaultSwitchKey, ""},
		{"literal string", "[switch]\nkey = 'ctrl+]'\n", "ctrl+]", ""},
		{"basic string with escape", "[switch]\nkey = \"ctrl+\\\\\"\n", `ctrl+\`, ""},
		{"refused key", "[switch]\nkey = \"ctrl+m\"\n", "", "Tab or Enter"},
		{"not a ctrl key", "[switch]\nkey = \"F12\"\n", "", "want ctrl+"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(tt.text)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || c.Switch.Key != tt.want {
				t.Errorf("key = %q, %v; want %q", c.Switch.Key, err, tt.want)
			}
		})
	}
}
