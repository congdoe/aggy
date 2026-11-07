//go:build unix

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSwitchWithBrokenRulesStays: the next launch would refuse, so the CLI on
// screen is not closed, and aggy says why.
func TestSwitchWithBrokenRulesStays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, w *world)
	}{
		{"rules file", func(t *testing.T, w *world) {
			if err := os.WriteFile(filepath.Join(w.home, ".aggy", "permissions.json"), []byte(`{"permissions":{"deny":["Bash(git push *)"],}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"this repo's section", func(t *testing.T, w *world) {
			real, err := filepath.EvalSymlinks(w.cwd)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"repos": map[string]any{real: map[string]any{"deny": []string{"Bash("}}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.home, ".aggy", "permissions.json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"aggy's config", func(t *testing.T, w *world) {
			if err := os.WriteFile(filepath.Join(w.home, ".aggy", "config.toml"), []byte("[switch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"the harness turned off", func(t *testing.T, w *world) {
			if err := os.WriteFile(filepath.Join(w.home, ".aggy", "config.toml"), []byte("[vendors.grok]\noff = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"the folder gone", func(t *testing.T, w *world) {
			if err := os.RemoveAll(w.cwd); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { switchStays(t, tc.breakIt) })
	}
}

func switchStays(t *testing.T, breakIt func(t *testing.T, w *world)) {
	w := newWorld(t)
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	gi := exec.Command("git", "init", "-q", ".")
	gi.Dir = w.cwd
	gi.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1"}
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
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
	breakIt(t, w) // after the pick, so the picker offered the vendor
	s.confirmPastGuard(time.Now())
	s.waitWithin([]byte("aggy did not switch"), offerTimeout)
	if runs := interactiveRuns(t, w); len(runs) != 1 || runs[0].Vendor != "claude" {
		t.Errorf("runs: %+v", runs)
	}
}
