//go:build unix

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/tooldef"
)

// fakeToolHook is a hook program whose answer is its first argument.
const fakeToolHook = `#!/bin/sh
/bin/cat >/dev/null
case "$1" in
allow) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"rtk ls"}}}' ;;
proxy) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"rtk proxy ls","dangerouslyDisableSandbox":true}}}' ;;
deny) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}' ;;
big) printf '{"hookSpecificOutput":{"permissionDecisionReason":"'; /usr/bin/head -c 70000 /dev/zero | /usr/bin/tr '\0' a; echo '"}}' ;;
bad) echo 'not json' ;;
fail) echo '{"hookSpecificOutput":{"permissionDecision":"deny"}}'; exit 1 ;;
slow) /bin/sleep 5; echo '{"hookSpecificOutput":{"permissionDecision":"deny"}}' ;;
dnt) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"dnt='"$DO_NOT_TRACK"'"}}' ;;
env) echo '{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"home='"$CLAUDE_CONFIG_DIR$CODEX_HOME$GROK_HOME"'"}}' ;;
esac
`

// TestHookTool: aggy hook-tool passes a tool hook's answer on only
// through the filter: an allow is stripped, a deny passes, and too big,
// invalid, failed, or slow output passes nothing; a changed definition
// runs nothing.
func TestHookTool(t *testing.T) {
	isolate(t)
	t.Setenv("PATH", "/usr/bin:/bin") // hook-tool rewrites it
	prog := filepath.Join(t.TempDir(), "fakehook")
	writeAt(t, prog, fakeToolHook, 0o755)
	t.Setenv("CLAUDE_CONFIG_DIR", "/claude-home")
	t.Setenv("CODEX_HOME", "/codex-home")
	rewrites := true
	tool := func(arg string) tooldef.Tool {
		h := tooldef.Hook{Vendors: []adapter.Vendor{adapter.Claude}, Event: "PreToolUse", Decides: true, Timeout: 1,
			Argv: []string{prog, arg}}
		if rewrites {
			h.Rewrites, h.RewritePrefix = true, "rtk "
		}
		return tooldef.Tool{BuiltIn: true, Digest: strings.Repeat("ab", 32), Def: tooldef.Def{Name: "demo", Hooks: []tooldef.Hook{h}}}
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
		hookToolWith(context.Background(), args, strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`), &out)
		return out.String()
	}
	for _, c := range []struct{ arg, want string }{
		{"allow", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"rtk ls"}}}`},
		{"deny", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`},
		// R80-1: a rewrite that is not the prefix plus the payload's
		// command passes nothing.
		{"proxy", ""},
		{"big", ""}, {"bad", ""}, {"fail", ""},
		// L78-7: no vendor home reaches a tool's hook.
		{"env", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"home="}}`},
	} {
		if got := run(c.arg); got != c.want {
			t.Errorf("%s: %q, want %q", c.arg, got, c.want)
		}
	}
	// T6822: DO_NOT_TRACK=1 over the person's own value, unless the
	// user config allows telemetry.
	t.Setenv("DO_NOT_TRACK", "0")
	dnt := func(v string) string {
		return `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"dnt=` + v + `"}}`
	}
	if got := run("dnt"); got != dnt("1") {
		t.Errorf("default: %q", got)
	}
	writeAt(t, filepath.Join(os.Getenv(home.Env), "config.toml"), "[tools]\nallow_telemetry = true\n", 0o600)
	// The person's own value passes (L1 of #97).
	if got := run("dnt"); got != dnt("0") {
		t.Errorf("allowed: %q", got)
	}
	// H78-1: a hook that does not declare rewrites never rewrites.
	rewrites = false
	if got := run("allow"); got != "" {
		t.Errorf("an undeclared rewrite passed: %q", got)
	}
	rewrites = true
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

// TestHookToolPathOutsideRepo (F74-2): a hook's bare program is looked
// up, and its PATH built, without the entries in the repo the CLI runs
// in.
func TestHookToolPathOutsideRepo(t *testing.T) {
	isolate(t)
	repo := switchRepo(t, "")
	outside := t.TempDir()
	answer := func(who string) string {
		return "#!/bin/sh\n/bin/cat >/dev/null\necho '{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"" + who + ":'\"$PATH\"'\"}}'\n"
	}
	writeAt(t, filepath.Join(repo, "node_modules", ".bin", "pathhook"), answer("repo"), 0o755)
	writeAt(t, filepath.Join(outside, "pathhook"), answer("outside"), 0o755)
	t.Setenv("PATH", filepath.Join(repo, "node_modules", ".bin")+":"+outside)
	t.Chdir(repo)
	old := hookTools
	t.Cleanup(func() { hookTools = old })
	hookTools = func() ([]tooldef.Tool, error) {
		h := tooldef.Hook{Vendors: []adapter.Vendor{adapter.Claude}, Event: "PreToolUse", Decides: true, Timeout: 5, Argv: []string{"pathhook"}}
		return []tooldef.Tool{{BuiltIn: true, Digest: strings.Repeat("ab", 32), Def: tooldef.Def{Name: "demo", Hooks: []tooldef.Hook{h}}}}, nil
	}
	var out bytes.Buffer
	hookToolWith(context.Background(), []string{"--vendor", "claude", "demo", "0", strings.Repeat("ab", 8)},
		strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`), &out)
	if want := `"permissionDecisionReason":"outside:` + outside + `"`; !strings.Contains(out.String(), want) {
		t.Errorf("got %q, want %s", out.String(), want)
	}
}

// TestToolPath (F74-2): the PATH a tool program gets leaves out the work
// tree and its main checkout (a linked worktree nested in it inherits
// its .envrc), and outside a repo the folder itself, unless that is the
// home folder.
func TestToolPath(t *testing.T) {
	isolate(t)
	main := switchRepo(t, "")
	wt := filepath.Join(main, ".claude", "worktrees", "w")
	runGit(t, main, "worktree", "add", "-q", wt)
	plain := t.TempDir()
	keep := t.TempDir()
	t.Setenv("PATH", strings.Join([]string{filepath.Join(main, "bin"), filepath.Join(wt, "bin"), filepath.Join(plain, "bin"), keep}, ":"))
	for _, c := range []struct{ dir, want string }{
		{wt, filepath.Join(plain, "bin") + ":" + keep},
		{main, filepath.Join(plain, "bin") + ":" + keep},
		{plain, filepath.Join(main, "bin") + ":" + filepath.Join(wt, "bin") + ":" + keep},
	} {
		if got, err := toolPath(c.dir); err != nil || got != c.want {
			t.Errorf("toolPath(%s) = %q, %v, want %q", c.dir, got, err, c.want)
		}
	}
	t.Setenv("HOME", plain)
	if got, _ := toolPath(plain); !strings.Contains(got, filepath.Join(plain, "bin")) {
		t.Errorf("home's own entries dropped: %q", got)
	}
	if _, err := toolPath(""); err == nil {
		t.Error("no folder passed")
	}
}
