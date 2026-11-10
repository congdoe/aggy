//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestReviewAttacks (PR #124 review): the command lines the review found
// starting a CLI with none of aggy's policy or gates, piped as a script would
// run them. Each is now refused before anything runs or is recorded (no fake
// run, no index), or runs with aggy's policy on the CLI's command line.
func TestReviewAttacks(t *testing.T) {
	const home = "HOMEDIR"
	bypass := `{"permissions":{"defaultMode":"bypassPermissions"}}`
	tests := []struct {
		name string
		env  []string
		args []string
		// code is the refusal's reason code; "" means the run goes ahead with
		// the policy, which policy checks.
		code   string
		policy func(t *testing.T, r fakecli.Record) bool
	}{
		{"claude -nv with bypass settings", nil, []string{"claude", "-nv", "--settings", bypass, "do it"}, "cli.flag_needs_no_policy", nil},
		{"bare aggy -nv", nil, []string{"-nv", "do it"}, "", claudePolicy},
		{"claude a list then --version", nil, []string{"claude", "--allowedTools", "--version", "do it"}, "", claudePolicy},
		{"grok -pv", nil, []string{"grok", "-pv"}, "", grokPolicy},
		{"grok -p --add-dir home", nil, []string{"grok", "-p", "x", "--add-dir", home}, "cli.flag_needs_no_policy", nil},
		{"codex exec --add-dir home", nil, []string{"codex", "exec", "--add-dir", home, "x"}, "cli.flag_needs_no_policy", nil},
		{"codex --worktree --add-dir home", nil, []string{"codex", "--worktree", "--add-dir", home, "x"}, "cli.flag_needs_no_policy", nil},
		{"a borrow child's aggy -p", []string{"AGGY_DELEGATED=1"}, []string{"-p", "hi"}, "borrow.one_hop", nil},
		{"--no-policy from a model's shell", []string{"AGGY_VENDOR=claude", "AGGY_SOCKET=/x"}, []string{"claude", "--no-policy", "-p", "--dangerously-skip-permissions", "hi"}, "supervisor.no_terminal", nil},
		{"bare aggy, empty stdin", nil, nil, "supervisor.no_terminal", nil},
		{"the typo claud", nil, []string{"claud"}, "cli.usage", nil},
		{"the typo dcotor", nil, []string{"dcotor"}, "cli.usage", nil},
		{"settings that skip trust and set an endpoint", nil, []string{"claude", "-p", "--settings", `{"enableAllProjectMcpServers":true,"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1"},"allowManagedHooksOnly":true}`, "hi"}, "cli.usage", nil},
		{"claude --environment", nil, []string{"claude", "--environment", "e1", "--settings", bypass, "do it"}, "cli.flag_clash", nil},
		{"print with a bad permissions file", nil, []string{"-p", "hi"}, "permissions.bad_file", nil},
		{"claude agents in a bypass mode", nil, []string{"claude", "agents", "--permission-mode", "bypassPermissions", "--dangerously-skip-permissions"}, "cli.usage", nil},
		{"a refused subcommand in print mode", nil, []string{"claude", "-p", "ultrareview"}, "cli.usage", nil},
		{"a refused subcommand piped", nil, []string{"claude", "rc"}, "cli.usage", nil},
		// PR #124 re-review.
		{"claude -- agents in a bypass mode", nil, []string{"claude", "--", "agents", "--permission-mode", "bypassPermissions", "--dangerously-skip-permissions"}, "cli.usage", nil},
		{"bare -- agents in a bypass mode", nil, []string{"--", "agents", "--permission-mode", "bypassPermissions"}, "cli.usage", nil},
		{"claude -p -- mcp serve", nil, []string{"claude", "-p", "--", "mcp", "serve"}, "cli.usage", nil},
		{"claude mcp -- serve", nil, []string{"claude", "mcp", "--", "serve"}, "cli.usage", nil},
		// A later -- is mcp add's own: it runs as typed, with no policy, even
		// from a session's tool shell (what stops a model there is the guard
		// hook, not this path).
		{"claude mcp add with its command", []string{"AGGY_VENDOR=claude", "AGGY_SOCKET=/x"}, []string{"claude", "mcp", "add", "evil", "--", "sh", "-c", "id"}, "",
			asTyped("mcp", "add", "evil", "--", "sh", "-c", "id")},
		{"the typo sesions with more words", nil, []string{"sesions", "list"}, "cli.usage", nil},
		{"the typo borow with more words", nil, []string{"borow", "research", "x"}, "cli.usage", nil},
		{"codex exec --skip-git-repo-check", nil, []string{"codex", "exec", "--skip-git-repo-check", "x"}, "cli.flag_needs_no_policy", nil},
		{"settings with MCP approvals, headers, a proxy login, and helpers", nil, []string{"claude", "-p", "--settings",
			`{"enabledMcpjsonServers":["evil"],"env":{"ANTHROPIC_CUSTOM_HEADERS":"Authorization: Bearer x","HTTPS_PROXY":"http://u:p@h:1","NODE_OPTIONS":"--require /tmp/x.js"},"otelHeadersHelper":"/tmp/x.sh","awsAuthRefresh":"x"}`, "hi"}, "cli.usage", nil},
		{"claude --rc then a bypass", nil, []string{"claude", "--rc", "--dangerously-skip-permissions"}, "cli.flag_needs_no_policy", nil},
		{"claude -dp", nil, []string{"claude", "-dp", "hi"}, "", claudePolicy},
		{"grok -p --no-ask-user --fs-write", nil, []string{"grok", "-p", "hi", "--no-ask-user", "--fs-write", "/"}, "cli.flag_needs_no_policy", nil},
		{"a borrow child's mcp add", []string{"AGGY_DELEGATED=1"}, []string{"claude", "mcp", "add", "evil", "--", "sh"}, "borrow.one_hop", nil},
		// PR #124 second re-review: a "--" the CLI takes as a flag's value.
		{"--name -- --settings bypass", nil, []string{"claude", "--name", "--", "--settings", bypass, "do it"}, "cli.usage", nil},
		{"print --debug-file -- --settings bypass", nil, []string{"claude", "-p", "--debug-file", "--", "--settings", bypass, "do it"}, "cli.usage", nil},
		{"-n -- --verbose agents", nil, []string{"claude", "-n", "--", "--verbose", "agents"}, "cli.usage", nil},
		{"bare --name -- --verbose mcp serve", nil, []string{"--name", "--", "--verbose", "mcp", "serve"}, "cli.usage", nil},
		{"-- --verbose agents", nil, []string{"claude", "--", "--verbose", "agents"}, "cli.usage", nil},
		{"-- a blank word then agents", nil, []string{"claude", "--", "", "agents"}, "cli.usage", nil},
		{"-- AGENTS", nil, []string{"claude", "--", "AGENTS"}, "cli.usage", nil},
		{"--name -- --permission-mode", nil, []string{"claude", "--name", "--", "--permission-mode"}, "cli.usage", nil},
		{"-- --settings bypass", nil, []string{"claude", "--", "--settings", bypass}, "cli.usage", nil},
		{"mcp SERVE", nil, []string{"claude", "mcp", "SERVE"}, "cli.usage", nil},
		{"mcp --x serve", nil, []string{"claude", "mcp", "--x", "serve"}, "cli.usage", nil},
		{"mcp add x -- npx y", nil, []string{"claude", "mcp", "add", "x", "--", "npx", "y"}, "", asTyped("mcp", "add", "x", "--", "npx", "y")},
		{"-- sessions list", nil, []string{"--", "sessions", "list"}, "", claudePolicy},
		{"print -p --, -p a switch in the help", nil, []string{"claude", "-p", "--", "hi"}, "", claudePolicy},
		// Rule C: every --settings is merged and checked, wherever it stands.
		{"a second --settings", nil, []string{"claude", "-p", "--settings", "{}", "--settings", bypass, "hi"}, "cli.flag_needs_no_policy", nil},
		{"--settings after an unknown flag", nil, []string{"claude", "-p", "--verbose", "--settings", bypass, "hi"}, "cli.flag_needs_no_policy", nil},
		{"--settings after a value flag", nil, []string{"claude", "-p", "--name", "--settings", bypass, "hi"}, "cli.flag_needs_no_policy", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.env = tt.env
			args := slices.Clone(tt.args)
			for i, a := range args {
				if a == home {
					args[i] = w.home
				}
			}
			if tt.name == "print with a bad permissions file" {
				if err := os.MkdirAll(filepath.Join(w.home, ".aggy"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(w.home, ".aggy", "permissions.json"), []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sc := map[string]any{"stdout": "ran\n", "interactive_without_tty": true,
				"extra_switches": []string{"-nv", "-pv", "--environment", "-dp", "--skip-git-repo-check", "--no-ask-user", "--rc", "--verbose"},
				"extra_flags":    []string{"--fs-write", "--name"}}
			code, out, errb := w.pipe(t, sc, "", args...)
			var recs []fakecli.Record
			if _, err := os.Stat(w.record); err == nil {
				recs = fakecli.Records(t, w.record)
			}
			if tt.code != "" {
				if code == 0 || len(recs) != 0 || !strings.Contains(errb, "["+tt.code+" · run ") {
					t.Fatalf("exit %d, runs %d, stderr %q; want refused with %s", code, len(recs), errb, tt.code)
				}
				if _, err := os.Stat(filepath.Join(w.home, ".aggy", "aggy.db")); err == nil {
					t.Errorf("the refused run created the index")
				}
				return
			}
			if code != 0 || out != "ran\n" || len(recs) != 1 || !tt.policy(t, recs[0]) {
				t.Fatalf("exit %d, stdout %q, stderr %q, runs %+v; want one run with aggy's policy", code, out, errb, recs)
			}
		})
	}
}

// claudePolicy says r carries aggy's deny rules in its --settings layer.
func claudePolicy(t *testing.T, r fakecli.Record) bool {
	return slices.Contains(settingsDeny(t, r), "Bash(rm -rf*)")
}

// TestGateExitCodeOnScreen (PR #124 re-review): a gate refusal exits 2 on
// screen, as it does in print mode (TestReviewAttacks) and for a subcommand.
func TestGateExitCodeOnScreen(t *testing.T) {
	w := newWorld(t)
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "codex", "--add-dir", w.home)
	s.waitFor([]byte("cli.flag_needs_no_policy"))
	if code := s.exit(); code != 2 {
		t.Errorf("exit %d, want 2: %s", code, s.screen())
	}
	if _, err := os.Stat(w.record); err == nil {
		for _, r := range fakecli.Records(t, w.record) {
			t.Errorf("the CLI ran: %+v", r)
		}
	}
}

// asTyped says r ran with exactly args: a subcommand aggy passes as typed.
func asTyped(args ...string) func(*testing.T, fakecli.Record) bool {
	return func(t *testing.T, r fakecli.Record) bool {
		if !slices.Equal(r.Args, args) {
			t.Errorf("args = %q, want %q", r.Args, args)
			return false
		}
		return true
	}
}

// grokPolicy says r carries aggy's deny rules as --deny.
func grokPolicy(_ *testing.T, r fakecli.Record) bool { return len(r.Flags["--deny"]) > 0 }
