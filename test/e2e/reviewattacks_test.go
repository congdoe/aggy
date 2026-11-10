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
			sc := map[string]any{"stdout": "ran\n", "interactive_without_tty": true, "extra_switches": []string{"-nv", "-pv", "--environment"}}
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

// grokPolicy says r carries aggy's deny rules as --deny.
func grokPolicy(_ *testing.T, r fakecli.Record) bool { return len(r.Flags["--deny"]) > 0 }
