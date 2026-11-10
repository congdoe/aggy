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
	"time"
)

// TestPolicyAcrossSwitch (T6967): the safety policy reaches each CLI of a
// session as that CLI can express it, before and after a switch. Claude asks,
// through aggy's decision hook (T6976); Grok, with no ask flag, denies; a
// group the person set to deny is a deny on both.
func TestPolicyAcrossSwitch(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(filepath.Join(w.home, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.home, ".aggy", "permissions.json"),
		[]byte(`{"permissions":{"allow":["Bash(rm -rf dist)"]},"policy":{"reset-hard":"deny"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
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
	s.send([]byte{0x1c})
	s.waitFor([]byte("aggy: switch vendor"))
	s.mu.Lock()
	key := []byte("2")
	if bytes.Contains(s.out.Bytes(), []byte("1  grok")) {
		key = []byte("1")
	}
	s.mu.Unlock()
	s.send(key)
	s.waitWithin([]byte("-> grok. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
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

	vals := runs[0].Flags["--settings"]
	if len(vals) != 1 {
		t.Fatalf("claude --settings = %q", vals)
	}
	var layer struct {
		Permissions struct{ Ask, Deny []string } `json:"permissions"`
		Hooks       map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(vals[0]), &layer); err != nil {
		t.Fatal(err)
	}
	p := layer.Permissions
	// On screen the asks are aggy's decision hook's (T6976), which asks for
	// every policy match but an exact pre-approval: none are static rules but
	// the guards on Claude's settings files (PR #129 review).
	for _, a := range p.Ask {
		if !strings.Contains(a, ".claude/settings") {
			t.Errorf("claude has a policy ask beside the decision hook: %v", p.Ask)
		}
	}
	if !slices.Contains(p.Ask, "Bash(*.claude/settings*)") || !slices.Contains(p.Ask, "Edit(//**/.claude/settings.local.json)") {
		t.Errorf("claude lacks the settings guards: %v", p.Ask)
	}
	hooked := false
	for _, g := range layer.Hooks["PreToolUse"] {
		for _, h := range g.Hooks {
			hooked = hooked || strings.Contains(h.Command, " hook decide --vendor claude --plan ")
		}
	}
	if !hooked {
		t.Errorf("claude lacks the decision hook: %+v", layer.Hooks)
	}
	if !slices.Contains(p.Deny, "Bash(git reset --hard*)") {
		t.Errorf("the group set to deny: deny %v", p.Deny)
	}

	deny := runs[1].Flags["--deny"]
	for _, r := range []string{"Bash(git clean *)", "Bash(git reset --hard*)", "Read(**/.env)", "Read(**/*.pem)"} {
		if !slices.Contains(deny, r) {
			t.Errorf("grok denies lack %s: %v", r, deny)
		}
	}
	// The deny stays, and the allow that names it does not reach Grok.
	if !slices.Contains(deny, "Bash(rm -rf*)") {
		t.Errorf("an allow removed a grok deny: %v", deny)
	}
	for _, a := range runs[1].Flags["--allow"] {
		if strings.Contains(a, "rm -rf") {
			t.Errorf("an allow naming a policy rule reached grok: %v", runs[1].Flags["--allow"])
		}
	}
}
