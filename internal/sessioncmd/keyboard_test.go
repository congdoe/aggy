package sessioncmd

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestKeyboardOnlyClosed (R121): no line Parse takes reaches a change (a
// confirmation, the tool list, tool on|off, trust, permissions, accept, reject,
// check) without KeyboardOnly, so a prompt hook, which any process in the
// CLI's tree can send, never runs one. Random lines from the words that matter.
func TestKeyboardOnlyClosed(t *testing.T) {
	words := []string{"aggy", "accept", "reject", "check", "tool", "on", "off", "status", "trust", "--opencode", "permissions", "show", "list", "add", "9f2", "--yes", "--check", "rtk", ".", "inbox", "--all", "diag", "switch", "grok", "--brief", "brief", "log", "-n", "x"}
	r := rand.New(rand.NewSource(1))
	n := 0
	for range 50000 {
		k := 1 + r.Intn(4)
		var parts []string
		for range k {
			parts = append(parts, words[r.Intn(len(words))])
		}
		sep := " "
		if r.Intn(4) == 0 {
			sep = "  "
		}
		for _, v := range []adapter.Vendor{adapter.Codex, adapter.Claude} {
			for _, pre := range Prefixes(v) {
				line := string(pre) + strings.Join(parts, sep)
				c, ok := Parse(v, line)
				if !ok {
					continue
				}
				n++
				change := len(c.Confirm) > 0 || c.Kind == ToolList || c.Sub == "tool" || c.Sub == "trust" || c.Sub == "permissions" ||
					c.Kind == Run && len(c.Args) > 0 && (c.Args[0] == "accept" || c.Args[0] == "reject" || c.Args[0] == "check") && (len(c.Args) != 2 || c.Args[1] != "--help")
				if change && !c.KeyboardOnly {
					t.Fatalf("%q -> %+v is not keyboard-only", line, c)
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no line parsed")
	}
}
