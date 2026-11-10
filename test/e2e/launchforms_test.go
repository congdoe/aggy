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

// settingsRules is the ask and deny lists of the --settings layer aggy
// passed: on screen the safety policy asks where Claude can (PR #125), so an
// on-screen launch's policy is in either list.
func settingsRules(t *testing.T, r fakecli.Record) []string {
	t.Helper()
	v := r.Flags["--settings"]
	if len(v) == 0 {
		return nil
	}
	var layer struct {
		Permissions struct{ Ask, Deny []string } `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(v[len(v)-1]), &layer); err != nil {
		t.Fatalf("--settings %q: %v", v[len(v)-1], err)
	}
	return append(layer.Permissions.Ask, layer.Permissions.Deny...)
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
			// Hand-edited for PR #125: on screen the policy asks, so look in
			// both lists (settingsRules), not only the deny list.
			if len(r.Flags["--session-id"]) != 1 || !slices.Contains(settingsRules(t, r), "Bash(rm -rf*)") {
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

// TestFlagPassThrough (T6962): a flag of the CLI's that aggy listed as refused,
// or does not know, reaches the CLI on screen as typed, with aggy's own launch
// flags, and the prompt after it stays the prompt.
func TestFlagPassThrough(t *testing.T) {
	tests := []struct {
		name string
		args []string
		flag string
		want []string
	}{
		{"a value flag aggy lists", []string{"claude", "--max-turns", "3", "fix it"}, "--max-turns", []string{"3"}},
		{"a switch aggy does not know", []string{"claude", "--zap-mode", "fix it"}, "--zap-mode", []string{""}},
		{"bare, a value flag", []string{"--max-turns=4", "fix it"}, "--max-turns", []string{"4"}},
		{"the appended prompt", []string{"claude", "--append-system-prompt", "be brief", "fix it"}, "--append-system-prompt", []string{"be brief"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			sc := map[string]any{"raw_echo": true, "stdout": "READY>", "extra_flags": []string{"--max-turns", "--append-system-prompt"}, "extra_switches": []string{"--zap-mode"}}
			s := w.start(t, sc, tt.args...)
			s.waitFor([]byte("READY>"))
			s.send([]byte{0x04})
			if code := s.exit(); code != 0 {
				t.Fatalf("exit = %d\n%s", code, s.screen())
			}
			r := interactiveRecord(t, w)
			if !slices.Equal(r.Flags[tt.flag], tt.want) || !slices.Equal(r.Positional, []string{"fix it"}) {
				t.Errorf("%s = %q, positional %q; args %q", tt.flag, r.Flags[tt.flag], r.Positional, r.Args)
			}
			// Hand-edited for PR #125: on screen the policy may ask.
			if len(r.Flags["--session-id"]) != 1 || !slices.Contains(settingsRules(t, r), "Bash(rm -rf*)") {
				t.Errorf("aggy's own launch flags: %q", r.Args)
			}
		})
	}
}

// TestSettingsMerged (T6962): the person's --settings, inline or a file, is
// merged with aggy's own layer into the one --settings Claude gets: their
// allow rules and aggy's deny rules both hold, on screen and in print mode.
func TestSettingsMerged(t *testing.T) {
	const user = `{"permissions":{"allow":["Bash(ls)"],"deny":["Read(./secret)"]},"model":"x"}`
	tests := []struct {
		name  string
		file  bool
		print bool
	}{
		{"inline, on screen", false, false},
		{"a file, on screen", true, false},
		{"inline, print mode", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			val := user
			if tt.file {
				if err := os.WriteFile(filepath.Join(w.cwd, "mine.json"), []byte(user), 0o600); err != nil {
					t.Fatal(err)
				}
				val = "mine.json"
			}
			var r fakecli.Record
			if tt.print {
				code, out, errb := w.pipe(t, map[string]any{"stdout": "ok\n", "interactive_without_tty": true}, "", "claude", "-p", "--settings", val, "hi")
				if code != 0 || out != "ok\n" {
					t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
				}
				r = fakecli.Records(t, w.record)[0]
			} else {
				s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "claude", "--settings", val)
				s.waitFor([]byte("READY>"))
				s.send([]byte{0x04})
				if code := s.exit(); code != 0 {
					t.Fatalf("exit = %d\n%s", code, s.screen())
				}
				r = interactiveRecord(t, w)
			}
			if n := len(r.Flags["--settings"]); n != 1 {
				t.Fatalf("--settings given %d times: %q", n, r.Args)
			}
			if tt.file {
				// PR #124 review (hand-edited): a settings file's content stays
				// off argv; the merged layer goes in a 0600 file in aggy's run
				// directory, removed with the run, so only its path is seen.
				v := r.Flags["--settings"][0]
				if !strings.HasPrefix(v, filepath.Join(w.home, ".aggy", "run")+"/") || strings.Contains(strings.Join(r.Args, " "), "Bash(ls)") {
					t.Errorf("--settings %q, args %q: want a path in aggy's run directory and none of the file's content", v, r.Args)
				}
				return
			}
			var layer struct {
				Permissions struct{ Allow, Ask, Deny []string } `json:"permissions"`
				Model       string                              `json:"model"`
			}
			if err := json.Unmarshal([]byte(r.Flags["--settings"][0]), &layer); err != nil {
				t.Fatal(err)
			}
			// Hand-edited for PR #125: on screen the policy's rule is an ask,
			// in print mode a deny (planHeadless); the person's deny stays a
			// deny either way.
			deny := layer.Permissions.Deny
			policy := layer.Permissions.Ask
			if tt.print {
				policy = deny
			}
			if !slices.Contains(layer.Permissions.Allow, "Bash(ls)") || !slices.Contains(deny, "Read(./secret)") ||
				!slices.Contains(policy, "Bash(rm -rf*)") || layer.Model != "x" {
				t.Errorf("merged layer = %s", r.Flags["--settings"][0])
			}
		})
	}
}

// TestSubcommandPassThrough (T6963): a management subcommand of the CLI runs
// as typed, with nothing of aggy's added and no session recorded; its stdin,
// stdout, and exit code are the CLI's.
func TestSubcommandPassThrough(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"claude mcp", []string{"claude", "mcp", "list"}, []string{"mcp", "list"}},
		{"claude auth, a flag after it", []string{"claude", "auth", "status", "--json"}, []string{"auth", "status", "--json"}},
		{"codex login, aggy's flag name after it", []string{"codex", "login", "--no-policy"}, []string{"login", "--no-policy"}},
		{"grok update", []string{"grok", "update"}, []string{"update"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			sc := map[string]any{"stdout": "listed\n", "exit_code": 4, "interactive_without_tty": true,
				"extra_switches": []string{"--json", "--no-policy"}}
			code, out, errb := w.pipe(t, sc, "", tt.args...)
			if code != 4 || out != "listed\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
			}
			recs := fakecli.Records(t, w.record)
			if len(recs) != 1 {
				t.Fatalf("runs = %+v", recs)
			}
			if r := recs[0]; !slices.Equal(r.Args, tt.want) {
				t.Errorf("args = %q, want %q", r.Args, tt.want)
			}
			if c, out := w.aggyCode(t, "sessions"); c != 0 || strings.Contains(out, w.cwd) {
				t.Errorf("a session was recorded: %d\n%s", c, out)
			}
		})
	}
}

// TestProbeCached (T6964): a second launch of an unchanged CLI runs no help
// probe; its --version runs each time (PR #124 review).
func TestProbeCached(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"on screen", []string{"claude", "--max-turns", "2"}},
		{"bare", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			sc := map[string]any{"raw_echo": true, "stdout": "READY>", "extra_flags": []string{"--max-turns"}}
			// A probe runs with a scrubbed environment, so the fake cannot
			// record it: a wrapper in its place logs each --version and
			// --help to a fixed file, then runs the fake.
			real := filepath.Join(w.home, "real", "claude")
			if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(w.bin, "claude"), real); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(w.home, "probes.log")
			wrapper := "#!/bin/sh\ncase \" $* \" in *' --version '*|*' --help '*) echo \"$*\" >> '" + log + "';; esac\nexec '" + real + "' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(w.bin, "claude"), []byte(wrapper), 0o755); err != nil {
				t.Fatal(err)
			}
			// PR #124 review: --version runs on every launch, behind a shim the
			// binary does not change; only the help runs are cached.
			probes := func() int {
				b, err := os.ReadFile(log)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				return strings.Count(string(b), "--help")
			}
			launch := func() {
				t.Helper()
				s := w.start(t, sc, tt.args...)
				s.waitFor([]byte("READY>"))
				s.send([]byte{0x04})
				if code := s.exit(); code != 0 {
					t.Fatalf("exit = %d\n%s", code, s.screen())
				}
			}
			launch()
			first := probes()
			if first == 0 {
				t.Fatal("first launch: no probe ran")
			}
			launch()
			if n := probes(); n != first {
				t.Errorf("second launch ran %d help probes", n-first)
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
