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
// session as that CLI can express it, before and after a switch. Claude asks;
// Grok, with no ask flag, denies; a rule the person pre-approved in aggy's file
// no longer asks on Claude but stays a deny on Grok; a group the person set to
// deny is a deny on both.
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
	}
	if err := json.Unmarshal([]byte(vals[0]), &layer); err != nil {
		t.Fatal(err)
	}
	p := layer.Permissions
	for _, r := range []string{"Bash(git clean *)", "Bash(git push * main)", "Bash(git push --force*)", "Read(//**/.env)", "Read(//**/*.pem)", "Bash(git push --force*main*)"} {
		if !slices.Contains(p.Ask, r) {
			t.Errorf("claude asks lack %s: %v", r, p.Ask)
		}
	}
	if !slices.Contains(p.Deny, "Bash(git reset --hard*)") || slices.Contains(p.Ask, "Bash(git reset --hard*)") {
		t.Errorf("the group set to deny: ask %v, deny %v", p.Ask, p.Deny)
	}
	if slices.Contains(p.Ask, "Bash(rm -rf*)") || slices.Contains(p.Deny, "Bash(rm -rf*)") {
		t.Errorf("a pre-approved rule reached claude: ask %v, deny %v", p.Ask, p.Deny)
	}
	if !slices.Contains(p.Ask, "Bash(rm -fr*)") {
		t.Errorf("another spelling than the one named still asks: %v", p.Ask)
	}

	deny := runs[1].Flags["--deny"]
	for _, r := range []string{"Bash(git clean *)", "Bash(git reset --hard*)", "Read(**/.env)", "Read(**/*.pem)"} {
		if !slices.Contains(deny, r) {
			t.Errorf("grok denies lack %s: %v", r, deny)
		}
	}
	// Grok has no prompt behind a removed rule, so a pre-approval keeps its
	// deny, and the allow that names it does not reach Grok (review 2).
	if !slices.Contains(deny, "Bash(rm -rf*)") {
		t.Errorf("a pre-approval removed a grok deny: %v", deny)
	}
	for _, a := range runs[1].Flags["--allow"] {
		if strings.Contains(a, "rm -rf") {
			t.Errorf("an allow naming a policy rule reached grok: %v", runs[1].Flags["--allow"])
		}
	}
}
