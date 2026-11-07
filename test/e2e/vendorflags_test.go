//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestLooseningFlagNoted (T6903): a person's own bypass flag needs --no-policy,
// then reaches the CLI with a launch note and a record that names it.
func TestLooseningFlagNoted(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>", "extra_switches": []string{"--dangerously-skip-permissions"}}
	s := w.start(t, sc, "claude", "--dangerously-skip-permissions")
	s.waitFor([]byte("cli.flag_needs_no_policy"))
	if code := s.exit(); code != 2 {
		t.Errorf("without --no-policy: exit = %d, want 2", code)
	}
	if n := len(fakecli.Records(t, w.record)); n != 0 {
		t.Errorf("the CLI started %d times without --no-policy", n)
	}

	s = w.start(t, sc, "claude", "--no-policy", "--dangerously-skip-permissions")
	s.waitFor([]byte("loosen approvals"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("with --no-policy: exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	if !slices.Contains(recs[len(recs)-1].Args, "--dangerously-skip-permissions") {
		t.Errorf("args = %q", recs[len(recs)-1].Args)
	}
	code, out := w.aggyCode(t, "diag", "--json")
	if code != 0 || !strings.Contains(out, "loosening_flags") || !strings.Contains(out, "--dangerously-skip-permissions") {
		t.Errorf("diag = %d, %s", code, out)
	}
}

// TestResumeFlagAgrees (T6903): aggy -r with no id refuses when nothing here
// can be reopened, as aggy claude -r does, and starts nothing.
func TestResumeFlagAgrees(t *testing.T) {
	for _, args := range [][]string{{"-r"}, {"claude", "-r"}, {"--continue"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			w := newWorld(t)
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, args...)
			s.waitFor([]byte("cli.not_found"))
			if code := s.exit(); code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if n := len(fakecli.Records(t, w.record)); n != 0 {
				t.Errorf("the CLI started %d times", n)
			}
		})
	}
}

// TestSafeModeNoted (T6952): a flag worth a line at launch reaches the CLI
// without --no-policy, and the person sees the line.
func TestSafeModeNoted(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>", "extra_switches": []string{"--safe-mode"}}
	s := w.start(t, sc, "claude", "--safe-mode")
	s.waitFor([]byte("may not run"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	if !slices.Contains(recs[len(recs)-1].Args, "--safe-mode") {
		t.Errorf("args = %q", recs[len(recs)-1].Args)
	}
}

// TestAddDirHomeRefused (T6952): a link to $HOME is refused at launch, before
// the CLI starts.
func TestAddDirHomeRefused(t *testing.T) {
	w := newWorld(t)
	link := filepath.Join(t.TempDir(), "to-home")
	if err := os.Symlink(w.home, link); err != nil {
		t.Fatal(err)
	}
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "codex", "--add-dir", link)
	s.waitFor([]byte("cli.flag_needs_no_policy"))
	if code := s.exit(); code == 0 {
		t.Error("exit 0")
	}
	if n := len(fakecli.Records(t, w.record)); n != 0 {
		t.Errorf("the CLI started %d times", n)
	}
}

// TestAddDirDroppedOnReopen (T6952): --add-dir of $HOME with --no-policy holds for
// that launch, and a tool change reopens the session without it instead of
// refusing and dropping to the shell.
func TestAddDirDroppedOnReopen(t *testing.T) {
	w := newWorld(t)
	w.aggyIn(t, "setup", "--yes") // the rules file, which the reopen needs without --no-policy
	sc := map[string]any{
		"stdout":       "READY>",
		"extra_flags":  []string{"--add-dir"},
		"hook_command": []string{buildAggy(t), "hook", "--vendor", "codex"},
		"hooks": []map[string]any{{"event": "SessionStart", "payload": map[string]any{"session_id": "{session_id}", "source": "startup"}},
			{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}},
		"session_files": []map[string]any{{"path": "sessions/" + time.Now().Format("2006/01/02") + "/rollout-x-{session_id}.jsonl", "content": `{"type":"session_meta","payload":{"id":"{session_id}","source":"cli"}}` + "\n"}},
	}
	s := w.start(t, sc, "codex", "--no-policy", "--add-dir", w.home)
	s.waitFor([]byte("loosen approvals"))
	s.waitFor([]byte("READY>"))
	s.send([]byte("work\r"))
	s.waitFor([]byte("work"))
	// A turn end counts only once aggy has recorded the thread's id from the
	// first hooks, so a later turn ends the one the note waits for.
	time.Sleep(time.Second)
	s.send([]byte("again\r"))
	s.waitFor([]byte("again"))
	code, out := w.aggyCode(t, "tool", "on", "context7", "--vendor", "codex")
	if code != 0 || !strings.Contains(out, "Told 1 running aggy session") {
		t.Fatalf("aggy tool on: %d\n%s", code, out)
	}
	// Codex has no hold signal: aggy says so, and the person asks for the resume.
	s.waitWithin([]byte("Tools changed for codex"), offerTimeout)
	s.confirmPastGuard(time.Now())
	s.waitWithin([]byte("same session -> codex. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	deadline := time.Now().Add(timeout)
	for len(interactiveRuns(t, w)) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d\nterminal: %q", code, s.screen())
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 {
		t.Fatalf("runs = %+v\nterminal: %q", runs, s.screen())
	}
	if !slices.Contains(runs[0].Args, "--add-dir") || slices.Contains(runs[1].Args, "--add-dir") {
		t.Errorf("--add-dir: %q, then %q", runs[0].Args, runs[1].Args)
	}
}
