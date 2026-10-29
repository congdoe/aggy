//go:build unix

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/transcript"
)

// e2eSecret is a token-shaped string a brief must never carry.
const e2eSecret = "ghp_Wm4Kq9Zt2Lx7Bn3Rv8Hc1Jd6Fs5Pa0Ye2Gu"

// confirmPastGuard presses the switch key at a confirmation aggy opened
// on its own, once its guard (supervisor.confirmGuard, 500 ms) has
// surely passed: the confirmation was on screen by the time waitWithin
// returned, so its clock started before seen. The guard is wall time by
// design (S-2), so the wait is on the clock, measured from that event.
func (s *session) confirmPastGuard(seen time.Time) {
	s.t.Helper()
	for time.Since(seen) < 600*time.Millisecond {
		time.Sleep(10 * time.Millisecond)
	}
	s.send([]byte{0x1c})
}

// waitRuns waits until the fakes have recorded n interactive runs.
func (w *world) waitRuns(t *testing.T, s *session, n int) {
	t.Helper()
	for end := time.Now().Add(offerTimeout); len(interactiveRuns(t, w)) < n; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%d interactive runs never started; terminal: %q", n, s.screen())
		}
	}
}

// TestLimitSwitchWithBrief (T6683): the on-screen Claude reports its plan
// limit (the fake's rate_limit StopFailure hook and limit text); aggy
// offers the switch to a vendor with quota, the switch key confirms it,
// and the next CLI starts in the same parent told to read a brief file
// that holds the task file and Claude's turns, with every secret line
// dropped.
func TestLimitSwitchWithBrief(t *testing.T) {
	w := newWorld(t)
	w.aggyIn(t, "setup", "--yes")
	const taskCanary, turnCanary = "task-heron-canary-71", "turn-marmot-canary-72"
	task := "# Task\n\n" + taskCanary + "\n\ndeploy token " + e2eSecret + "\n"
	if err := os.MkdirAll(filepath.Join(w.cwd, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.cwd, ".aggy", "task.md"), []byte(task), 0o600); err != nil {
		t.Fatal(err)
	}
	// Claude's transcript, where internal/transcript reads it; the fake
	// writes it under every vendor's home, used only under Claude's.
	turns := `{"type":"user","message":{"content":"` + turnCanary + `"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"use GITHUB_TOKEN=` + e2eSecret + `"}]}}` + "\n"
	// Only Claude forwards its hooks: the next CLI, run with the same
	// scenario, reports a limit too, which this test does not follow.
	hook := `[ "$AGGY_VENDOR" = claude ] || exit 0; exec ` + aggySh(t) + ` hook --vendor claude "$1"`
	sc := map[string]any{
		"stdout":        "READY>",
		"error":         "rate_limit",
		"hook_command":  []string{"/bin/sh", "-c", hook, "sh"},
		"session_files": []map[string]any{{"path": "projects/" + transcript.ClaudeProjectDir(w.cwd) + "/{session_id}.jsonl", "content": turns}},
	}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("claude reports a limit (unverified)"), offerTimeout)
	s.waitWithin([]byte(". Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	w.waitRuns(t, s, 2)
	s.send([]byte("/exit\r"))
	// The next CLI, at its limit too, exits 1: aggy passes that on.
	if code := s.exit(); code != 1 {
		t.Fatalf("aggy exit = %d, want the last CLI's 1; terminal: %q", code, s.screen())
	}

	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[0].Vendor != "claude" || runs[1].Vendor == "claude" {
		t.Fatalf("interactive runs = %+v, want claude then another vendor", runs)
	}
	next := runs[1]
	if next.Env["AGGY_PARENT"] != runs[0].Env["AGGY_PARENT"] {
		t.Errorf("%s env %v: want claude's parent", next.Vendor, next.Env)
	}
	var path string
	for _, a := range next.Args {
		if p, ok := strings.CutPrefix(a, "Read the handoff brief at "); ok {
			path, _ = strings.CutSuffix(p, " before anything else, then continue the work it describes.")
			if a != adapter.BriefPrompt(path) {
				t.Errorf("prompt %q", a)
			}
		}
	}
	if filepath.Dir(path) != filepath.Join(w.cwd, ".aggy", "briefs") {
		t.Fatalf("%s args %q: no brief prompt in .aggy/briefs", next.Vendor, next.Args)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	brief := string(b)
	for _, want := range []string{"# Handoff brief", "running in claude", taskCanary, turnCanary} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	for _, leak := range []string{e2eSecret, e2eSecret[:12], "deploy token", "GITHUB_TOKEN"} {
		if strings.Contains(brief, leak) {
			t.Errorf("brief holds %q:\n%s", leak, brief)
		}
	}
	if gi, err := os.ReadFile(filepath.Join(filepath.Dir(path), ".gitignore")); err != nil || string(gi) != "*\n" {
		t.Errorf("briefs .gitignore = %q, %v", gi, err)
	}
	s.restored("after the switch")
}

// TestBorrowRoundTrip (T6683): the on-screen agent runs aggy borrow
// review in its own shell; a fake Codex child answers headless, locked
// down at the work-tree root, and the answer comes back to the agent,
// as the borrow's summary and the inbox report it names.
func TestBorrowRoundTrip(t *testing.T) {
	w := newWorld(t)
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
		c.Dir = w.cwd
		c.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(w.cwd, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "one")
	if err := os.WriteFile(filepath.Join(w.cwd, "a.go"), []byte("package a\n\nfunc Changed() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A borrow child's environment is scrubbed (execrun), so it never
	// gets the fakes' AGGY_FAKE_* from aggy's: codex on PATH becomes a
	// wrapper that sets them (the scenario file w.start writes) and runs
	// the fake, kept off PATH.
	fake := filepath.Join(w.home, "fake-codex")
	if err := os.Rename(filepath.Join(w.bin, "codex"), fake); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nAGGY_FAKE_VENDOR=codex " + fakecli.EnvScenario + "='" + filepath.Join(w.home, "scenario.json") + "' " +
		fakecli.EnvRecord + "='" + w.record + "' exec '" + fake + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(w.bin, "codex"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	w.aggyIn(t, "setup", "--yes")

	const answer = "review-wombat-answer-73"
	// The agent's shell command, run once at the session's start: only
	// the on-screen CLI borrows, never the child (the same scenario).
	borrow := `[ "$AGGY_VENDOR" = claude ] || exit 0; ` + aggySh(t) + ` borrow review "review the change" --vendor codex; echo "[borrow exit $?]"`
	sc := map[string]any{
		"stdout":       "READY>",
		"hook_command": []string{"/bin/sh", "-c", borrow, "sh"},
		"hooks":        []map[string]any{{"event": "SessionStart", "payload": map[string]any{}}},
		"events":       []map[string]any{{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": answer}}},
	}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("[borrow exit "), offerTimeout)
	// The agent gets the summary and the report's path; the report holds
	// the child's answer.
	_, rest, _ := strings.Cut(s.screen(), "Full report: ")
	report, _, _ := strings.Cut(rest, "\r\n")
	if !strings.Contains(s.screen(), "review on codex") || !strings.Contains(s.screen(), "[borrow exit 0]") ||
		!strings.Contains(s.screen(), answer) ||
		filepath.Dir(report) != filepath.Join(w.home, ".aggy", "inbox") {
		t.Fatalf("the agent got no report; terminal: %q", s.screen())
	}
	if b, err := os.ReadFile(report); err != nil || !strings.Contains(string(b), answer) {
		t.Fatalf("report %s lacks the answer (%v):\n%s", report, err, b)
	}
	s.waitFor([]byte("READY>"))
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}

	var on, child []string
	var parent string
	for _, r := range interactiveRuns(t, w) {
		on = append(on, r.Vendor)
		parent = r.Env["AGGY_PARENT"]
	}
	for _, r := range fakecli.Records(t, w.record) {
		if r.Mode != "headless" {
			continue
		}
		child = append(child, r.Vendor)
		if r.Cwd != w.cwd || r.Env["AGGY_DELEGATED"] == "" || r.Env["AGGY_SOCKET"] != "" {
			t.Errorf("child cwd %q env %v: want the work-tree root, delegated, no socket", r.Cwd, r.Env)
		}
		if !strings.Contains(r.Stdin, "+func Changed()") {
			t.Errorf("child prompt lacks the diff: %q", r.Stdin)
		}
	}
	if len(on) != 1 || on[0] != "claude" || len(child) != 1 || child[0] != "codex" || parent == "" {
		t.Fatalf("runs: on screen %q, children %q, parent %q", on, child, parent)
	}
}
