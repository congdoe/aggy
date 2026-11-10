//go:build unix

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAggySwitchCaught (T6978): /aggy switch grok, typed at the prompt with
// the stand-alone /switch turned off in [commands], is caught as /switch
// is: it never reaches Claude as a turn, and the switch goes to grok.
func TestAggySwitchCaught(t *testing.T) {
	w, sc, log := commandWorld(t)
	if err := os.WriteFile(filepath.Join(w.home, ".aggy", "config.toml"), []byte("[commands.switch]\noff = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	waitStops(t, log, 2) // the launch's and the turn's
	s.typeSlowly("/aggy switch grok")
	s.send([]byte("\r"))
	s.waitWithin([]byte("-> grok. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	w.waitRuns(t, s, 2)
	if got := stops(t, log); got != 2 {
		t.Errorf("Stop hooks = %d, want 2: the command reached Claude as a turn", got)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 2 || runs[1].Vendor != "grok" {
		t.Fatalf("interactive runs = %+v, want claude then grok", runs)
	}
	s.restored("after the switch")
}

// TestAggyTrustList (T6977, T6978): /aggy trust, typed at the prompt, opens
// the trust list on aggy's own screen; All and Enter save the answer, which
// records aggy's trust and Claude's own key for the folder, and nothing
// reaches Claude as a turn.
func TestAggyTrustList(t *testing.T) {
	w, sc, log := commandWorld(t)
	gi := exec.Command("/usr/bin/git", "init", "-q", w.cwd)
	gi.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1"}
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	claudeJSON := filepath.Join(w.home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{"numStartups": 3, "projects": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	waitStops(t, log, 2)
	s.typeSlowly("/aggy trust")
	s.send([]byte("\r"))
	s.waitWithin([]byte("All harnesses"), offerTimeout)
	time.Sleep(300 * time.Millisecond) // past the typed-ahead guard
	s.send([]byte("1"))
	time.Sleep(100 * time.Millisecond)
	s.send([]byte("\r"))
	s.waitWithin([]byte("Saved this folder's answer"), offerTimeout)
	s.send([]byte("\x1b"))
	time.Sleep(200 * time.Millisecond)
	if got := stops(t, log); got != 2 {
		t.Errorf("Stop hooks = %d, want 2: the command reached Claude as a turn", got)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	s.restored("after the list")

	b, err := os.ReadFile(filepath.Join(w.home, ".aggy", "trust.json"))
	if err != nil || !strings.Contains(string(b), w.cwd) {
		t.Fatalf("trust.json = %q, %v", b, err)
	}
	var cj struct {
		NumStartups int                        `json:"numStartups"`
		Projects    map[string]map[string]bool `json:"projects"`
	}
	b, err = os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.NumStartups != 3 || !cj.Projects[w.cwd]["hasTrustDialogAccepted"] {
		t.Fatalf(".claude.json = %s", b)
	}
}
