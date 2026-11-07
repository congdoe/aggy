package adapter

import "testing"

// TestLocalCommand: exact whole lines of the vendor's own table, and nothing else.
func TestLocalCommand(t *testing.T) {
	for _, tt := range []struct {
		v    Vendor
		line string
		want bool
	}{
		{Claude, "/clear", true},
		{Claude, "/model", true},
		{Claude, "/cost", true},
		{Claude, "/status", true},
		{Claude, "/help", true},
		{Claude, "/config", true},
		{Claude, "/permissions", true},
		{Claude, "/memory", true},
		{Claude, "/context", true},
		{Claude, "/usage", true},
		{Claude, "/compact", false},
		{Claude, "/clear now", false},
		{Claude, "/clear ", false},
		{Claude, "/CLEAR", false},
		{Claude, "/cle", false},
		{Claude, "/my-skill", false},
		{Claude, "", false},
		{Codex, "/clear", false},
		{Grok, "/clear", false},
		{OpenCode, "/clear", false},
	} {
		if got := LocalCommand(tt.v, tt.line); got != tt.want {
			t.Errorf("%s %q = %v", tt.v, tt.line, got)
		}
	}
}
