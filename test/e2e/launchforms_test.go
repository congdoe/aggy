//go:build unix

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// pipe runs aggy with stdin from a pipe, as a script or `git diff | aggy -p`
// does, against scenario, and returns its exit code, stdout, and stderr apart.
func (w *world) pipe(t *testing.T, scenario map[string]any, stdin string, args ...string) (int, string, string) {
	t.Helper()
	sc, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	scPath := filepath.Join(w.home, "scenario.json")
	if err := os.WriteFile(scPath, sc, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(buildAggy(t), args...)
	cmd.Dir = w.cwd
	cmd.Env = append([]string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin",
		fakecli.EnvScenario + "=" + scPath, fakecli.EnvRecord + "=" + w.record}, w.env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String(), errb.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, out.String(), errb.String()
}

// settingsDeny is the deny list of the --settings layer aggy passed, or nil.
func settingsDeny(t *testing.T, r fakecli.Record) []string {
	t.Helper()
	v := r.Flags["--settings"]
	if len(v) == 0 {
		return nil
	}
	var layer struct {
		Permissions struct{ Deny []string } `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(v[len(v)-1]), &layer); err != nil {
		t.Fatalf("--settings %q: %v", v[len(v)-1], err)
	}
	return layer.Permissions.Deny
}

// TestPromptArgument (T6961): a prompt on aggy's command line, with or without
// the vendor's name and its flags, starts the on-screen session with it, after
// aggy's own launch flags, as the CLI itself takes it.
func TestPromptArgument(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		model string
	}{
		{"the vendor's", []string{"claude", "fix the bug"}, ""},
		{"bare", []string{"fix the bug"}, ""},
		{"bare, with the CLI's flags", []string{"--model", "opus", "fix the bug"}, "opus"},
		{"after --, though it names a command", []string{"--", "fix the bug"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, tt.args...)
			s.waitFor([]byte("READY>"))
			s.send([]byte{0x04})
			if code := s.exit(); code != 0 {
				t.Fatalf("exit = %d\n%s", code, s.screen())
			}
			r := interactiveRecord(t, w)
			if r.Vendor != "claude" || len(r.Positional) != 1 || r.Positional[0] != "fix the bug" || r.Args[len(r.Args)-1] != "fix the bug" {
				t.Errorf("vendor %s, args %q, positional %q", r.Vendor, r.Args, r.Positional)
			}
			if got := r.Flags["--model"]; tt.model != "" && (len(got) != 1 || got[0] != tt.model) {
				t.Errorf("--model = %q, want %q", got, tt.model)
			}
			if len(r.Flags["--session-id"]) != 1 || len(settingsDeny(t, r)) == 0 {
				t.Errorf("aggy's own launch flags: %q", r.Args)
			}
		})
	}
}

// TestPrintModePipe (T6961): print mode with a piped stdin runs the CLI with
// no supervisor: its stdin, stdout, and exit code are the person's, aggy still
// adds its safety policy, and no session is recorded.
func TestPrintModePipe(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"bare -p", []string{"-p", "review this"}},
		{"the vendor's -p", []string{"claude", "-p", "review this"}},
		{"a pipe alone", []string{"review this"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			sc := map[string]any{"stdout": "the reply\nline two\n", "exit_code": 3, "interactive_without_tty": true}
			code, out, errb := w.pipe(t, sc, "diff --git a/x b/x\n", tt.args...)
			if code != 3 || out != "the reply\nline two\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
			}
			recs := fakecli.Records(t, w.record)
			if len(recs) != 1 {
				t.Fatalf("runs = %+v", recs)
			}
			r := recs[0]
			if r.StdinTerminal || !slices.Contains(r.Args, "review this") || len(r.Flags["--session-id"]) != 0 {
				t.Errorf("args %q, stdin terminal %v", r.Args, r.StdinTerminal)
			}
			if r.Mode == "headless" && r.Stdin != "diff --git a/x b/x\n" {
				t.Errorf("stdin = %q", r.Stdin)
			}
			if !slices.Contains(settingsDeny(t, r), "Bash(rm -rf*)") {
				t.Errorf("no safety policy: %q", r.Args)
			}
			if c, out := w.aggyCode(t, "sessions"); c != 0 || strings.Contains(out, w.cwd) {
				t.Errorf("a session was recorded: %d\n%s", c, out)
			}
		})
	}
}

// TestContinueWithFlags (T6961): aggy -c takes the CLI's flags too, sorted by
// the vendor of the session it reopens, as claude -c --model x does.
func TestContinueWithFlags(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{
		"raw_echo": true, "stdout": "READY>",
		"session_files": []map[string]any{{"path": "projects/p/{session_id}.jsonl", "content": "{}"}},
	}
	s := w.start(t, sc, "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy claude exit = %d", code)
	}
	first := fakecli.Records(t, w.record)[0]

	s = w.start(t, sc, "-c", "--model", "opus-x", "--permission-mode", "plan")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy -c exit = %d\n%s", code, s.screen())
	}
	recs := fakecli.Records(t, w.record)
	second := recs[len(recs)-1]
	if !slices.Equal(second.Flags["--resume"], first.Flags["--session-id"]) || !slices.Equal(second.Flags["--model"], []string{"opus-x"}) ||
		!slices.Equal(second.Flags["--permission-mode"], []string{"plan"}) || second.Env["AGGY_MODEL"] != "opus-x" {
		t.Errorf("aggy -c args = %q, want --resume of the first, the model, and the mode", second.Args)
	}
}
