//go:build unix

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// screenRecording reads a scrubbed recording of Claude Code 2.1.326's screen
// (testdata/fixtures/claude/screen) and cuts it at the first byte of until,
// or keeps it whole for "".
func screenRecording(t *testing.T, name, until string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude", "screen", name))
	if err != nil {
		t.Fatal(err)
	}
	if until != "" {
		i := bytes.Index(b, []byte(until))
		if i < 0 {
			t.Fatalf("%s holds no %q", name, until)
		}
		b = b[:i]
	}
	return string(b)
}

// firstTurnScreenWorld is a fresh Claude at 2.1.326 on a 30 by 100 terminal,
// set up with aggy's hooks, whose start-up draws stdout: at launch it runs
// its hooks once, a SessionStart, and no turn end.
func firstTurnScreenWorld(t *testing.T, stdout string) (*world, *session) {
	t.Helper()
	w := newWorld(t)
	w.rows, w.cols = 30, 100
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	script := `[ "$AGGY_VENDOR" = claude ] || exit 0; exec ` + aggySh(t) + ` hook --vendor claude "$1"`
	sc := map[string]any{
		"stdout":        stdout,
		"hook_command":  []string{"/bin/sh", "-c", script, "sh"},
		"hooks":         []map[string]any{{"event": "SessionStart", "payload": map[string]any{"session_id": "{session_id}", "source": "startup"}}},
		"session_files": startedFiles(),
	}
	claudeVersion(t, w, "2.1.326")
	s := w.start(t, sc, "run", "claude")
	return w, s
}

// TestFirstTurnScreenOneStep (T7016): the fake Claude draws the start-up
// screen Claude Code 2.1.326 drew live (its composer, empty, with the
// placeholder), and /switch codex typed in full goes in one step: aggy reads
// the screen, types /exit with no status-line question, and Codex starts.
func TestFirstTurnScreenOneStep(t *testing.T) {
	w, s := firstTurnScreenWorld(t, screenRecording(t, "claude.raw", "\x1b(B\x0f"))
	s.waitWithin([]byte("<filepath>"), offerTimeout)
	waitAccepted(t, w, "SessionStart", 1)
	s.typeSlowly("/switch codex")
	s.send([]byte("\r"))
	waitEvent(t, w, "switch.request.confirmed")
	w.waitRuns(t, s, 2)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 2 || runs[1].Vendor != "codex" {
		t.Fatalf("interactive runs = %d, want claude then codex", len(runs))
	}
	s.mu.Lock()
	asked := bytes.Contains(s.out.Bytes(), []byte("-> codex"))
	s.mu.Unlock()
	if asked {
		t.Error("the status line asked")
	}
}

// TestFirstTurnScreenTrustDialog (T7016): the fake Claude draws the
// folder-trust dialog Claude Code 2.1.326 drew live in a new folder ("❯ No,
// exit" selected). /switch codex typed in full asks on the status line, as
// before: aggy types nothing into the dialog.
func TestFirstTurnScreenTrustDialog(t *testing.T) {
	w, s := firstTurnScreenWorld(t, screenRecording(t, "claude-newfolder.raw", "\x1b(B\x0f"))
	s.waitWithin([]byte("\x1b[22Gfolder"), offerTimeout)
	waitAccepted(t, w, "SessionStart", 1)
	s.typeSlowly("/switch codex")
	s.send([]byte("\r"))
	s.waitWithin([]byte("-> codex"), offerTimeout)
	time.Sleep(300 * time.Millisecond)
	if runs := interactiveRuns(t, w); len(runs) != 1 {
		t.Fatalf("interactive runs = %d, want claude alone", len(runs))
	}
	s.send([]byte{0x1b}) // Esc: the switch is dropped
	time.Sleep(300 * time.Millisecond)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 1 {
		t.Fatalf("interactive runs = %d, want claude alone", len(runs))
	}
}
