//go:build unix

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
)

// openCodeWorldE2E is a world with fake-opencode installed as opencode and
// aggy setup run, so the picker offers it.
func openCodeWorldE2E(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	b, err := os.ReadFile(fakecli.Build(t).OpenCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.bin, "opencode"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real git repo, as a brief reads its status.
	gi := exec.Command("/usr/bin/git", "init", "-q", w.cwd)
	gi.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin"}
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	return w
}

// openCodeScenario has the fake report as aggy's plugin does: through aggy
// hook, from the CLI's own tree.
func openCodeScenario(t *testing.T, vendor string, events ...map[string]any) map[string]any {
	return map[string]any{"stdout": "READY>", "hook_command": []string{buildAggy(t), "hook", "--vendor", vendor}, "hooks": events}
}

func ocStop() map[string]any {
	return map[string]any{"event": "Stop", "payload": map[string]any{"sessionID": "{session_id}"}}
}

func ocConfig(t *testing.T, r fakecli.Record) (doc struct {
	Model        string   `json:"model"`
	Instructions []string `json:"instructions"`
	Permissions  []struct{ Action, Resource, Effect string }
	Policies     []struct{ Action, Resource, Effect string }
}) {
	t.Helper()
	if len(r.OpenCodeConfig) == 0 {
		t.Fatalf("no launch config in the record: %+v", r.Env)
	}
	if err := json.Unmarshal(r.OpenCodeConfig, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestOpenCodeLaunchUntrusted: aggy run opencode in a repo not trusted for
// it: mini, the design's variables with the inherited ones gone, and a note
// pointing to Claude Code.
func TestOpenCodeLaunchUntrusted(t *testing.T) {
	w := openCodeWorldE2E(t)
	w.env = append(w.env, "OPENCODE_CONFIG_PROJECT_DISABLE=0", "OPENCODE_CONFIG=/evil", "XAI_API_KEY=dummy", "ANTHROPIC_API_KEY=dummy", "GIT_CONFIG_COUNT=9")
	s := w.start(t, openCodeScenario(t, "opencode"), "run", "opencode")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.mu.Lock()
	shown := s.out.String()
	s.mu.Unlock()
	if !strings.Contains(shown, "opencode mini") || !strings.Contains(shown, "Claude Code") {
		t.Errorf("no note pointing to Claude Code: %q", shown)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	r := interactiveRuns(t, w)[0]
	if r.Args[0] != "mini" || !slices.Contains(r.Args, "--standalone") || len(r.Flags["--session"]) != 1 || !strings.HasPrefix(r.Flags["--session"][0], "ses_") {
		t.Errorf("args = %q", r.Args)
	}
	for k, want := range map[string]string{"OPENCODE_DISABLE_PROJECT_CONFIG": "1", "OPENCODE_DISABLE_AUTOUPDATE": "1", "PWD": w.cwd, "GIT_CONFIG_COUNT": "1",
		"GIT_CONFIG_KEY_0": "core.fsmonitor", "GIT_CONFIG_VALUE_0": "false", "AGGY_VENDOR": "opencode"} {
		if r.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, r.Env[k], want)
		}
	}
	if _, set := r.Env["OPENCODE_CONFIG_PROJECT_DISABLE"]; set {
		t.Error("a stale OPENCODE_CONFIG_PROJECT_DISABLE reached it: it beats the variable aggy sets")
	}
	if r.Env["OPENCODE_CONFIG"] == "/evil" || !filepath.IsAbs(r.Env["OPENCODE_CONFIG"]) {
		t.Errorf("OPENCODE_CONFIG = %q", r.Env["OPENCODE_CONFIG"])
	}
	for _, k := range []string{"XAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if slices.Contains(r.EnvKeys, k) {
			t.Errorf("%s reached the session", k)
		}
	}
	if slices.ContainsFunc(ocConfig(t, r).Permissions, func(p struct{ Action, Resource, Effect string }) bool {
		return p.Action == "read" && p.Effect == "allow"
	}) {
		t.Error("an untrusted repo got reads with no prompt")
	}
	// The launch's folder goes with it.
	if _, err := os.Stat(r.Env["OPENCODE_CONFIG"]); err == nil {
		t.Error("the launch's folder outlived the CLI")
	}
}

// TestOpenCodeLaunchTrusted: a trusted repo gets the full screen, and full
// auto where the person set it.
func TestOpenCodeLaunchTrusted(t *testing.T) {
	w := openCodeWorldE2E(t)
	state := filepath.Join(w.home, ".aggy")
	body, _ := json.Marshal(map[string][]string{"repos": {w.cwd}})
	if err := os.WriteFile(filepath.Join(state, "opencode-trust.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.toml"), []byte("[opencode]\nauto = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := w.start(t, openCodeScenario(t, "opencode"), "run", "opencode")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	r := interactiveRuns(t, w)[0]
	if r.Args[0] == "mini" || !slices.Contains(r.Args, "--auto") || !slices.Contains(r.Args, "--standalone") {
		t.Errorf("args = %q, want the full screen with --auto", r.Args)
	}
}

// TestOpenCodeSwitchAway: a switch from OpenCode at a turn end the plugin
// reports, with the brief read through session export.
func TestOpenCodeSwitchAway(t *testing.T) {
	w := openCodeWorldE2E(t)
	sc := openCodeScenario(t, "opencode", ocStop())
	sc["opencode_export"] = `{"info":{},"messages":[{"type":"user","text":"EXPORTED-USER-WORD","time":{"created":1794015495255}},
		{"type":"assistant","time":{"created":1794015495266},"content":[{"type":"text","text":"EXPORTED-ASSISTANT-WORD"}]}]}`
	s := w.start(t, sc, "run", "opencode")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	s.waitWithin([]byte("work"), offerTimeout)
	s.send([]byte{0x1c})
	s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
	pickVendor(t, s, "claude", true)
	s.waitWithin([]byte("brief -> claude. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	w.waitRuns(t, s, 2)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[0].Vendor != "opencode" || runs[1].Vendor != "claude" {
		t.Fatalf("runs = %+v", runs)
	}
	if runs[0].Env["AGGY_PARENT"] != runs[1].Env["AGGY_PARENT"] {
		t.Errorf("parents differ")
	}
	briefs, _ := filepath.Glob(filepath.Join(w.cwd, ".aggy", "briefs", "*.md"))
	if len(briefs) != 1 {
		t.Fatalf("briefs = %q", briefs)
	}
	b, err := os.ReadFile(briefs[0])
	if err != nil {
		t.Fatal(err)
	}
	// The export ran clean of the launch's rules and of inherited variables.
	var exports int
	for _, r := range fakecli.Records(t, w.record) {
		if r.Vendor == "opencode" && r.Mode == "export" {
			exports++
			if !slices.Contains(r.Args, "--standalone") || r.Env["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" {
				t.Errorf("export = %q, env %v", r.Args, r.Env)
			}
		}
	}
	if exports == 0 {
		t.Error("no session export ran")
	}
	if !strings.Contains(string(b), "EXPORTED-USER-WORD") || !strings.Contains(string(b), "EXPORTED-ASSISTANT-WORD") {
		t.Errorf("the brief has no turns from session export:\n%s", b)
	}
}

// TestOpenCodeSwitchTo: a switch from Claude reaches OpenCode with the
// brief as a copy in its instructions, and starts no turn: the launch
// counts as at its prompt.
func TestOpenCodeSwitchTo(t *testing.T) {
	w := openCodeWorldE2E(t)
	if err := os.WriteFile(filepath.Join(w.cwd, "CLAUDE.md"), []byte("REPO-CLAUDE-MD"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := map[string]any{"stdout": "READY>", "hook_command": []string{buildAggy(t), "hook", "--vendor", "claude"},
		"hooks": []map[string]any{{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}}}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	s.waitWithin([]byte("work"), offerTimeout)
	s.send([]byte{0x1c})
	s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
	pickVendor(t, s, "opencode", true)
	s.waitWithin([]byte("brief -> opencode. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	w.waitRuns(t, s, 2)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[1].Vendor != "opencode" {
		t.Fatalf("runs = %+v", runs)
	}
	r := runs[1]
	all := strings.Join(r.OpenCodeInstructions, "\n---\n")
	if !strings.Contains(all, "REPO-CLAUDE-MD") || !strings.Contains(all, "handoff brief") {
		t.Errorf("instructions = %q", r.OpenCodeInstructions)
	}
	if slices.ContainsFunc(r.Args, func(a string) bool { return strings.Contains(a, "brief") }) {
		t.Errorf("a brief reached argv: %q", r.Args)
	}
}

// TestOpenCodeNoExitOverDialog: with a permission open, aggy offers no
// switch at a turn end and types nothing.
func TestOpenCodeNoExitOverDialog(t *testing.T) {
	w := openCodeWorldE2E(t)
	sc := openCodeScenario(t, "opencode",
		map[string]any{"event": "PermissionAsked", "payload": map[string]any{"sessionID": "{session_id}", "requestID": "per_1"}}, ocStop())
	s := w.start(t, sc, "run", "opencode")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte{0x1c})
	s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
	pickVendor(t, s, "claude", false)
	time.Sleep(2 * switchQuietE2E)
	if s.screen(); strings.Contains(s.screen(), "-> claude. Ctrl+") {
		t.Fatalf("offered a switch with a permission open: %q", s.screen())
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 1 {
		t.Fatalf("runs = %+v: no switch should have gone through", runs)
	}
}

// switchQuietE2E is a little over supervisor.switchQuiet's default.
const switchQuietE2E = 1500 * time.Millisecond

// pickVendor presses the picker's digit for v, after b for a brief.
func pickVendor(t *testing.T, s *session, v string, brief bool) {
	t.Helper()
	deadline := time.Now().Add(offerTimeout)
	for time.Now().Before(deadline) {
		screen := s.screen()
		for _, n := range []string{"1", "2", "3", "4"} {
			if strings.Contains(screen, n+"  "+v) {
				keys := []byte(n)
				if brief {
					keys = []byte("b" + n)
				}
				s.send(keys)
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s is not in the picker: %q", v, s.screen())
}

// TestOpenCodeGuardProcess: the plugin's check, as the process it runs.
func TestOpenCodeGuardProcess(t *testing.T) {
	w := openCodeWorldE2E(t)
	run := func(payload map[string]any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(payload)
		cmd := exec.Command(buildAggy(t), "hook", "opencode-guard")
		cmd.Dir = w.cwd
		cmd.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader(string(b))
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	base := map[string]any{"tool": "shell", "directory": w.cwd, "root": w.cwd}
	with := func(k, v string) map[string]any {
		m := map[string]any{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	if code, out := run(with("command", "go test ./...")); code != 0 || out != "" {
		t.Errorf("an ordinary command: %d %q", code, out)
	}
	if code, out := run(with("command", "PATH=/x:$PATH; git status")); code != 2 || !strings.Contains(out, "[guard.refused · run ") {
		t.Errorf("a declaration: %d %q", code, out)
	}
	if code, out := run(with("command", "opencode auth export")); code != 2 || !strings.Contains(out, "sign-in") {
		t.Errorf("auth export: %d %q", code, out)
	}
	cmd := exec.Command(buildAggy(t), "hook", "opencode-guard")
	cmd.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy")}
	cmd.Stdin = strings.NewReader("not json")
	if err := cmd.Run(); err == nil {
		t.Error("an unreadable payload passed: the guard must fail closed")
	}
}
