//go:build unix

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// toolDecisions are the fake claude's tool_decision records, in order.
func toolDecisions(t *testing.T, w *world) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(w.record)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(l, &m) == nil && m["mode"] == "tool_decision" {
			out = append(out, m)
		}
	}
	return out
}

// TestPolicyHookDecisions (T6976): an on-screen Claude launch carries aggy's
// decision hook in place of the policy's static asks, and the fake claude,
// which runs PreToolUse hooks and rules as Claude's docs say, gets: allow for
// the exact command pre-approved with aggy permissions add, ask for the same
// command joined to another, for other policy matches, and for a payload aggy
// cannot read, deny for a group set to deny, and nothing for a command no rule
// matches. In dontAsk mode every ask is a deny. The diagnostic log holds a
// start and an end for each call and none of the commands.
func TestPolicyHookDecisions(t *testing.T) {
	w := newWorld(t)
	gi := exec.Command("/usr/bin/git", "init", "-q", w.cwd)
	gi.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin"}
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	state := filepath.Join(w.home, ".aggy")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	// Trusted, so an allow that is not read-only applies (aggy trust's file
	// for OpenCode counts as trust too).
	body, _ := json.Marshal(map[string][]string{"repos": {w.cwd}})
	if err := os.WriteFile(filepath.Join(state, "opencode-trust.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	const canary = "lynx-canary-91"
	approved := "git push --force origin main"
	if err := os.WriteFile(filepath.Join(state, "permissions.json"),
		[]byte(`{"permissions":{"allow":["Bash(`+approved+`)","Bash(git push *)"]},"policy":{"reset-hard":"deny"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + state, "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	bash := func(cmd string) map[string]any {
		return map[string]any{"tool": "Bash", "input": map[string]any{"command": cmd}}
	}
	notJSON := "not json " + canary
	calls := []map[string]any{
		bash(approved),
		bash(approved + " && echo " + canary),
		bash("git status"),
		bash("rm -rf " + canary),
		{"tool": "Read", "input": map[string]any{"file_path": "/" + canary + "/.env"}},
		bash("git reset --hard " + canary),
		{"tool": "Bash", "raw": notJSON},
		// PR #129 review: Monitor runs a shell command, and a write to a
		// Claude settings file asks.
		{"tool": "Monitor", "input": map[string]any{"command": "rm -rf " + canary, "description": "x"}},
		{"tool": "Write", "input": map[string]any{"file_path": filepath.Join(w.cwd, ".claude", "settings.local.json"), "content": "{}"}},
		{"tool": "WebFetch", "input": map[string]any{"url": "https://example.com/" + canary}},
	}
	want := []string{"allow", "ask", "none", "ask", "ask", "deny", "ask", "ask", "ask", "none"}
	sc := map[string]any{"stdout": "READY>", "tool_calls": calls, "extra_values": map[string][]string{"--permission-mode": {"dontAsk"}}}

	runOnce := func(args ...string) []map[string]any {
		t.Helper()
		_ = os.Remove(w.record)
		s := w.start(t, sc, args...)
		s.waitWithin([]byte("READY>"), offerTimeout)
		s.send([]byte("/exit\r"))
		if code := s.exit(); code != 0 {
			t.Fatalf("aggy %v exit = %d", args, code)
		}
		runs := interactiveRuns(t, w)
		if len(runs) != 1 {
			t.Fatalf("runs %+v", runs)
		}
		var layer struct {
			Permissions struct{ Ask, Deny []string } `json:"permissions"`
			Hooks       map[string][]struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command   string `json:"command"`
					OnFailure string `json:"onFailure"`
					Timeout   int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(runs[0].Flags["--settings"][0]), &layer); err != nil {
			t.Fatal(err)
		}
		// Only the guards on Claude's settings files stay static (PR #129).
		for _, a := range layer.Permissions.Ask {
			if !strings.Contains(a, ".claude/settings") {
				t.Errorf("static policy asks are still there: %q", layer.Permissions.Ask)
			}
		}
		if !strings.Contains(strings.Join(layer.Permissions.Ask, " "), "Bash(*.claude/settings*)") {
			t.Errorf("the settings guards are missing: %q", layer.Permissions.Ask)
		}
		if !strings.Contains(strings.Join(layer.Permissions.Deny, " "), "Bash(git reset --hard*)") {
			t.Errorf("the group set to deny is not a static deny: %q", layer.Permissions.Deny)
		}
		pre := layer.Hooks["PreToolUse"]
		if len(pre) == 0 || !strings.Contains(pre[len(pre)-1].Hooks[0].Command, " hook decide --vendor claude --plan ") ||
			pre[len(pre)-1].Hooks[0].OnFailure != "block" || pre[len(pre)-1].Hooks[0].Timeout == 0 {
			t.Errorf("PreToolUse %+v", pre)
		}
		return toolDecisions(t, w)
	}

	got := runOnce("run", "claude")
	if len(got) != len(want) {
		t.Fatalf("decisions %v", got)
	}
	for i, d := range got {
		if d["decision"] != want[i] {
			t.Errorf("call %d (%v): %v, want %s (hooks %v)", i, calls[i], d["decision"], want[i], d["answers"])
		}
	}

	// dontAsk: what asked is refused.
	got = runOnce("claude", "--permission-mode", "dontAsk")
	for i, d := range got {
		w := want[i]
		if w == "ask" {
			w = "deny"
		}
		if d["decision"] != w {
			t.Errorf("dontAsk call %d: %v, want %s", i, d["decision"], w)
		}
	}

	exp := exec.Command(buildAggy(t), "diag", "export", "--jsonl")
	exp.Dir = w.cwd
	exp.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + state, "PATH=" + w.bin + ":/usr/bin:/bin"}
	out, err := exp.Output()
	if err != nil {
		t.Fatalf("diag export: %v", err)
	}
	if strings.Contains(string(out), canary) || strings.Contains(string(out), approved) {
		t.Error("the diagnostic log holds a command or path")
	}
	starts, ends := strings.Count(string(out), `"event":"hook.decision.started"`), strings.Count(string(out), `"event":"hook.decision.ended"`)
	if starts != 2*len(calls) || ends != starts {
		t.Errorf("%d starts and %d ends, want %d each", starts, ends, 2*len(calls))
	}
}
