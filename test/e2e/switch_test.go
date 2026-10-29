//go:build unix

package e2e

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// TestSwitchAtTurnEnd (T6664): a vendor picked mid-session waits for the
// CLI's Stop hook (sent through aggy hook from the CLI's own tree), asks,
// types /exit on the switch key, and launches the next CLI in the same parent.
func TestSwitchAtTurnEnd(t *testing.T) {
	w := newWorld(t)
	// aggy setup records the vendors the picker offers.
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	sc := map[string]any{
		"stdout":       "READY>",
		"hook_command": []string{buildAggy(t), "hook", "--vendor", "claude"},
		"hooks":        []map[string]any{{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}},
	}
	s := w.start(t, sc, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte("work\r")) // a turn; the fake's Stop hook ends it
	s.waitFor([]byte("work"))
	s.send([]byte{0x1c})
	s.waitFor([]byte("aggy: switch vendor"))
	s.mu.Lock()
	key := []byte("2")
	if bytes.Contains(s.out.Bytes(), []byte("1  grok")) {
		key = []byte("1")
	}
	s.mu.Unlock()
	s.send(append([]byte("b"), key...)) // with a brief (T6683)
	s.waitWithin([]byte("brief -> grok. Ctrl+"), offerTimeout)
	time.Sleep(600 * time.Millisecond) // past the confirmation's guard
	s.send([]byte{0x1c})
	// grok is on screen: end it.
	deadline := time.Now().Add(timeout)
	for len(interactiveRuns(t, w)) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	runs := interactiveRuns(t, w)
	if len(runs) != 2 || runs[0].Vendor != "claude" || runs[1].Vendor != "grok" {
		t.Fatalf("interactive runs = %+v, want claude then grok", runs)
	}
	if runs[1].Env["AGGY_PARENT"] != runs[0].Env["AGGY_PARENT"] || runs[1].Env["AGGY_VENDOR"] != "grok" {
		t.Errorf("grok env %v: want the same parent and AGGY_VENDOR=grok", runs[1].Env)
	}
	if len(runs[1].Flags["--session-id"]) != 1 {
		t.Errorf("grok args %q: want a new pre-assigned session", runs[1].Args)
	}
	briefs, _ := filepath.Glob(filepath.Join(w.cwd, ".aggy", "briefs", "*.md"))
	if len(briefs) != 1 || !slices.Contains(runs[1].Args, adapter.BriefPrompt(briefs[0])) {
		t.Errorf("grok args %q: want the prompt to read the brief in %q", runs[1].Args, briefs)
	}
	s.restored("after the switch")
}

// TestSkillRequestedSwitch (T6668): aggy switch, run from inside the
// CLI's tree as a skill would, queues a switch the user confirms.
func TestSkillRequestedSwitch(t *testing.T) {
	w := newWorld(t)
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	// The fake runs it as a hook: the "event" grok is the argument. Under
	// grok itself the request is refused, which must not fail the fake.
	sc := map[string]any{
		"stdout":       "READY>",
		"hook_command": []string{"/bin/sh", "-c", buildAggy(t) + " switch \"$1\" >/dev/null 2>&1 || true", "sh"},
		"hooks":        []map[string]any{{"event": "grok", "payload": map[string]any{}}},
	}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("-> grok. Ctrl+"), offerTimeout)
	time.Sleep(600 * time.Millisecond) // past the confirmation's guard
	s.send([]byte{0x1c})
	deadline := time.Now().Add(timeout)
	for len(interactiveRuns(t, w)) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 2 || runs[1].Vendor != "grok" {
		t.Fatalf("interactive runs = %+v, want claude then grok", runs)
	}
}

// interactiveRuns returns the fake's records of interactive runs.
func interactiveRuns(t *testing.T, w *world) []fakecli.Record {
	t.Helper()
	var out []fakecli.Record
	for _, r := range fakecli.Records(t, w.record) {
		if r.Mode == "interactive" {
			out = append(out, r)
		}
	}
	return out
}
