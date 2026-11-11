package sessioncmd

import (
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestRuleChanges (T6979): /aggy permissions add|remove allow|ask|deny
// "<rule>" runs aggy permissions for this folder, keyboard only, its page
// showing the command's own question answered no and y answering it yes;
// --everywhere, any other kind, and the rest of permissions are not taken.
func TestRuleChanges(t *testing.T) {
	for _, c := range []struct {
		line string
		ok   bool
		args []string
	}{
		{`/aggy permissions add deny "Bash(rm -rf *)"`, true, []string{"permissions", "add", "deny", "Bash(rm -rf *)"}},
		{`/aggy permissions add ask Bash(git push)`, true, []string{"permissions", "add", "ask", "Bash(git push)"}},
		{`/aggy permissions add allow "Read(./docs/**)"`, true, []string{"permissions", "add", "allow", "Read(./docs/**)"}},
		{`/aggy permissions remove allow "Bash(npm test)"`, true, []string{"permissions", "remove", "allow", "Bash(npm test)"}},
		{`$aggy permissions remove deny "Bash(x)"`, true, []string{"permissions", "remove", "deny", "Bash(x)"}},
		{`/aggy permissions add deny "Bash(x)" --everywhere`, false, nil},
		{`/aggy permissions add deny --everywhere`, false, nil},
		{`/aggy permissions add nope "Bash(x)"`, false, nil},
		{`/aggy permissions add deny`, false, nil},
		{`/aggy permissions add denyx "Bash(x)"`, false, nil},
		{`/aggy permissions add deny "a"b"`, false, nil},
	} {
		on := adapter.Claude
		if c.line[0] == '$' {
			on = adapter.Codex
		}
		got, ok := Parse(on, c.line)
		if ok && got.Kind != Run {
			ok = false // a helper question or a list is not the change
		}
		if ok != c.ok {
			t.Errorf("Parse(%q) ok = %v (%+v)", c.line, ok, got)
			continue
		}
		if !ok {
			continue
		}
		if !slices.Equal(got.Args, c.args) || !got.KeyboardOnly || !slices.Equal(got.Confirm, got.Args) || !got.Answer || got.ConfirmIf != Asked {
			t.Errorf("Parse(%q) = %+v", c.line, got)
		}
	}
	for _, line := range []string{"/aggy permissions", "/aggy permissions list", "/aggy permissions show"} {
		if c, ok := Parse(adapter.Claude, line); !ok || c.Confirm != nil {
			t.Errorf("Parse(%q) = %+v, want the read-only page", line, c)
		}
	}
}
