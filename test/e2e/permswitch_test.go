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

// TestSwitchWithBrokenRulesStays: the next launch would refuse, so the CLI on
// screen is not closed, and aggy says why.
func TestSwitchWithBrokenRulesStays(t *testing.T) {
	w := newWorld(t)
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
	s.send([]byte("work\r"))
	s.waitFor([]byte("work"))
	pf := filepath.Join(w.home, ".aggy", "permissions.json")
	if err := os.WriteFile(pf, []byte(`{"permissions":{"deny":["Bash(git push *)"],}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.send([]byte{0x1c})
	s.waitFor([]byte("aggy: switch vendor"))
	s.mu.Lock()
	key := []byte("2")
	if bytes.Contains(s.out.Bytes(), []byte("1  grok")) {
		key = []byte("1")
	}
	s.mu.Unlock()
	s.send(append([]byte("b"), key...))
	s.waitWithin([]byte("brief -> grok. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	s.waitWithin([]byte("aggy did not switch"), offerTimeout)
	if runs := interactiveRuns(t, w); len(runs) != 1 || runs[0].Vendor != "claude" {
		t.Errorf("runs: %+v", runs)
	}
}
