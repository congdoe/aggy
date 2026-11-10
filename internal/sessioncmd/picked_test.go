package sessioncmd

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestPicked (T6975): a command a prompt hook reports goes with the person's
// keys only when what they typed before the menu keys starts its name (one
// letter at least past the /), and the arguments they typed are its own.
func TestPicked(t *testing.T) {
	for _, c := range []struct {
		typed  string
		tabbed bool
		after  string
		line   string
		want   bool
	}{
		{"/s", false, "", "/switch", true},
		{"/sw", false, "", "/switch", true},
		{"/switch", false, "", "/switch", true},
		{"/ag", false, "", "/aggy", true},
		{"/in", true, "", "/inbox", true},
		{"/in", true, "", "/inbox ", true},
		{"/sw", true, "grok", "/switch grok", true},
		{"/sw", true, " grok ", "/switch  grok", true},
		{"/switch grok", false, "", "/switch grok", true},
		{"/ag", true, "doctor", "/aggy doctor", true},
		// A bare / is a prefix of every command, the person's own too.
		{"/", false, "", "/switch", false},
		{"/", true, "", "/aggy", false},
		{"", false, "", "/aggy", false},
		// The person's own commands name nothing aggy's.
		{"/deploy", false, "", "/aggy", false},
		{"/review", false, "", "/aggy doctor", false},
		{"/compact", false, "", "/switch grok", false},
		{"/clear", false, "", "/inbox", false},
		{"/s", false, "", "/aggy", false},
		// Arguments must be what was typed.
		{"/sw", true, "grok", "/switch codex", false},
		{"/sw", true, "", "/switch codex", false},
		{"/sw", false, "", "/switch grok", false},
		{"/ag", false, "", "/aggy doctor", false},
		// A space ends the word: it must be whole, and no Tab follows it.
		{"/sw grok", false, "", "/switch grok", false},
		{"/switch grok", true, "", "/switch grok", false},
		{"/switch", false, "", "/switchx", true}, // a prefix while no space ends it
		{"/switchx", false, "", "/switch", false},
		{"/ sw", false, "", "/switch", false},
		{"$sw", false, "", "/switch", false},
		{"/sw", false, "", "/ switch", false},
		{"/sw", false, "", "", false},
	} {
		if got := Picked(adapter.Claude, c.typed, c.tabbed, c.after, c.line); got != c.want {
			t.Errorf("Picked(%q, %v, %q, %q) = %v, want %v", c.typed, c.tabbed, c.after, c.line, got, c.want)
		}
	}
	if !Picked(adapter.Codex, "$sw", false, "", "$switch") || Picked(adapter.Grok, "$sw", false, "", "$switch") {
		t.Error("the prefix is the vendor's own")
	}
}

// TestToolOnAsks (T6975): /aggy tool on keeps a y step for a question its first
// run asks (a first index's y/N), answering it yes; off asks nothing.
func TestToolOnAsks(t *testing.T) {
	c, ok := Parse(adapter.Claude, "/aggy tool on codegraph")
	if !ok || !slices.Equal(c.Confirm, []string{"tool", "on", "codegraph"}) || !c.Answer || c.ConfirmIf != Asked || !c.KeyboardOnly {
		t.Fatalf("tool on: %+v %v", c, ok)
	}
	if c, ok := Parse(adapter.Claude, "/aggy tool off codegraph"); !ok || len(c.Confirm) > 0 || c.ConfirmIf != "" {
		t.Fatalf("tool off: %+v %v", c, ok)
	}
}
