//go:build unix

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/tooldef"
)

// fakeToolHook is a hook program whose answer is its first argument.
const fakeToolHook = `#!/bin/sh
/bin/cat >/dev/null
case "$1" in
allow) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"rtk ls"}}}' ;;
deny) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}' ;;
big) printf '{"hookSpecificOutput":{"permissionDecisionReason":"'; /usr/bin/head -c 70000 /dev/zero | /usr/bin/tr '\0' a; echo '"}}' ;;
bad) echo 'not json' ;;
fail) echo '{"hookSpecificOutput":{"permissionDecision":"deny"}}'; exit 1 ;;
slow) /bin/sleep 5; echo '{"hookSpecificOutput":{"permissionDecision":"deny"}}' ;;
esac
`

// TestHookTool: aggy hook-tool passes a tool hook's answer on only
// through the filter: an allow is stripped, a deny passes, and too big,
// invalid, failed, or slow output passes nothing; a changed definition
// runs nothing.
func TestHookTool(t *testing.T) {
	isolate(t)
	prog := filepath.Join(t.TempDir(), "fakehook")
	writeAt(t, prog, fakeToolHook, 0o755)
	tool := func(arg string) tooldef.Tool {
		return tooldef.Tool{BuiltIn: true, Digest: strings.Repeat("ab", 32), Def: tooldef.Def{Name: "demo",
			Hooks: []tooldef.Hook{{Vendors: []adapter.Vendor{adapter.Claude}, Event: "PreToolUse", Decides: true, Timeout: 1,
				Argv: []string{prog, arg}}}}}
	}
	run := func(arg string, args ...string) string {
		t.Helper()
		old := hookTools
		hookTools = func() ([]tooldef.Tool, error) { return []tooldef.Tool{tool(arg)}, nil }
		defer func() { hookTools = old }()
		if args == nil {
			args = []string{"--vendor", "claude", "demo", "0", strings.Repeat("ab", 8)}
		}
		var out bytes.Buffer
		hookToolWith(context.Background(), args, strings.NewReader(`{"tool_input":{"command":"ls"}}`), &out)
		return out.String()
	}
	for _, c := range []struct{ arg, want string }{
		{"allow", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"rtk ls"}}}`},
		{"deny", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`},
		{"big", ""}, {"bad", ""}, {"fail", ""},
	} {
		if got := run(c.arg); got != c.want {
			t.Errorf("%s: %q, want %q", c.arg, got, c.want)
		}
	}
	start := time.Now()
	if got := run("slow"); got != "" || time.Since(start) > 4*time.Second {
		t.Errorf("slow: %q after %s", got, time.Since(start))
	}
	// Another digest, hook index, or vendor runs nothing.
	for _, args := range [][]string{
		{"--vendor", "claude", "demo", "0", strings.Repeat("cd", 8)},
		{"--vendor", "claude", "demo", "1", strings.Repeat("ab", 8)},
		{"--vendor", "codex", "demo", "0", strings.Repeat("ab", 8)},
		{"--vendor", "claude", "nope", "0", strings.Repeat("ab", 8)},
	} {
		if got := run("deny", args...); got != "" {
			t.Errorf("%q: %q", args, got)
		}
	}
}
