package fakecli

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Regression tests for the PR #4 review (C5, S5). Each fails on 522a431.

func TestFakesCheckValues(t *testing.T) {
	fakes := Build(t)
	tests := []struct {
		name     string
		vendor   string
		scenario string
		stdin    string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"session id not a uuid", "claude", "", "p", []string{"-p", "--session-id", "abc"}, 2, "UUID"},
		{"session id uuid", "grok", "", "", []string{"-p", "--session-id", abc}, 0, ""},
		{"output format not recorded", "claude", "", "p", []string{"-p", "--output-format", "yaml"}, 2, "invalid value"},
		{"permission mode not recorded", "grok", "", "", []string{"-p", "--permission-mode", "yolo"}, 2, "invalid value"},
		{"sandbox not recorded", "codex", "", "p", []string{"exec", "--sandbox", "danger-full-access"}, 2, "invalid value"},
		{"sandbox added by scenario", "codex", `{"extra_values":{"--sandbox":["workspace-write"]}}`, "p", []string{"exec", "--sandbox", "workspace-write"}, 0, ""},
		{"claude -p without a prompt", "claude", "", "", []string{"-p"}, 2, "no prompt"},
		{"claude -p with an argument prompt", "claude", "", "", []string{"-p", "hello"}, 0, ""},
		{"codex exec without a prompt", "codex", "", "", []string{"exec", "--json"}, 2, "no prompt"},
		{"codex exec resume without a prompt", "codex", "", "", []string{"exec", "resume", "t-1"}, 2, "no prompt"},
		{"interactive without a terminal", "claude", "", "", nil, 2, "needs a terminal"},
		{"interactive opted out", "claude", `{"interactive_without_tty":true}`, "", nil, 0, ""},
		{"resume and session id, lax", "grok", "", "", []string{"-p", "--resume", "r", "--session-id", abc}, 0, ""},
		{"resume and session id, strict", "grok", `{"strict":["resume-with-session-id"]}`, "", []string{"-p", "--resume", "r", "--session-id", abc}, 2, "fork-session"},
		{"resume, session id, and fork, strict", "grok", `{"strict":["resume-with-session-id"]}`, "", []string{"-p", "--resume", "r", "--session-id", abc, "--fork-session"}, 0, ""},
		{"grok continue", "grok", `{"interactive_without_tty":true}`, "", []string{"--continue"}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, fakes)
			r := e.run(t, tt.vendor, tt.scenario, tt.stdin, tt.args...)
			if r.code != tt.wantCode || !strings.Contains(r.stderr, tt.wantErr) {
				t.Errorf("exit %d, stderr %q; want %d and %q", r.code, r.stderr, tt.wantCode, tt.wantErr)
			}
		})
	}
}

func TestFakesRefuseRealHomeSpellings(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skip("no user home")
	}
	fakes := Build(t)
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(u.HomeDir, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  []string
	}{
		{"HOME is a symlink to the real home", []string{"HOME=" + link}},
		{"relative CODEX_HOME", []string{"CODEX_HOME=codex"}},
		{"relative GROK_HOME", []string{"GROK_HOME=./g"}},
		{"CODEX_HOME elsewhere in the real home", []string{"CODEX_HOME=" + filepath.Join(u.HomeDir, "aggy-fake-should-not-exist")}},
		{"CLAUDE_CONFIG_DIR through a link to the real home", []string{"CLAUDE_CONFIG_DIR=" + filepath.Join(link, ".claude")}},
	}
	if runtime.GOOS == "darwin" {
		tests = append(tests, struct {
			name string
			env  []string
		}{"HOME in another case", []string{"HOME=" + strings.ToUpper(u.HomeDir)}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, fakes)
			e.vars = append(e.vars, tt.env...)
			// No scenario: the fake checks HOME before it reads or writes
			// anything, and even an old fake would write nothing here.
			if r := e.run(t, "codex", "", "task", "exec"); r.code != 97 {
				t.Errorf("exit %d (stderr %q), want 97", r.code, r.stderr)
			}
		})
	}
}
