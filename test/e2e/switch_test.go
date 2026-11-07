//go:build unix

package e2e

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	s.confirmPastGuard(time.Now())
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
		"hook_command": askThenStop(t, adapter.Claude, ""),
		"hooks": []map[string]any{{"event": "grok", "payload": map[string]any{}},
			{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}},
	}
	s := w.start(t, sc, "run", "claude")
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
	if runs := interactiveRuns(t, w); len(runs) != 2 || runs[1].Vendor != "grok" {
		t.Fatalf("interactive runs = %+v, want claude then grok", runs)
	}
}

// TestSwitchNoTurnYet (T6893): aggy types no exit command into a CLI that
// has ended no turn since it launched, a new session or a resumed one: it
// may be showing a dialog. It says so; the CLI quit by hand completes the
// switch, unless the user cancels it first.
func TestSwitchNoTurnYet(t *testing.T) {
	const note = "will not type /exit into it"
	setup := func(t *testing.T, w *world) {
		t.Helper()
		c := exec.Command(buildAggy(t), "setup", "--yes")
		c.Dir = w.cwd
		c.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("aggy setup: %v\n%s", err, out)
		}
	}
	// pick presses the switch key and takes grok from the list.
	pick := func(s *session) {
		s.t.Helper()
		s.send([]byte{0x1c})
		s.waitFor([]byte("aggy: switch vendor"))
		key := []byte("2")
		if strings.Contains(s.screen(), "1  grok") {
			key = []byte("1")
		}
		s.send(key)
	}
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}

	t.Run("quit by hand switches", func(t *testing.T) {
		w := newWorld(t)
		setup(t, w)
		s := w.start(t, sc, "run", "claude")
		s.waitFor([]byte("READY>"))
		pick(s)
		s.waitWithin([]byte(note), offerTimeout)
		if strings.Contains(s.screen(), "-> grok. Ctrl+") {
			t.Error("a confirmation was offered")
		}
		// The note closes on any other key, which goes to the CLI: the
		// raw_echo fake would show an exit command typed into it.
		s.send([]byte("k"))
		s.waitFor([]byte("k"))
		if strings.Contains(s.screen(), "/exit\r") {
			t.Errorf("the exit command reached the CLI: %q", s.screen())
		}
		s.send([]byte{0x04}) // Ctrl+D: the fake quits, as the person would
		w.waitRuns(t, s, 2)
		s.send([]byte{0x04})
		if code := s.exit(); code != 0 {
			t.Fatalf("aggy exit = %d", code)
		}
		runs := interactiveRuns(t, w)
		if len(runs) != 2 || runs[0].Vendor != "claude" || runs[1].Vendor != "grok" || runs[1].Env["AGGY_PARENT"] != runs[0].Env["AGGY_PARENT"] {
			t.Fatalf("interactive runs = %+v, want claude then grok in one parent", runs)
		}
		s.restored("after the switch")
	})
	t.Run("a resumed session cancels", func(t *testing.T) {
		w := newWorld(t)
		setup(t, w)
		s := w.start(t, sc, "run", "claude")
		s.waitFor([]byte("READY>"))
		s.send([]byte{0x04})
		s.exit()
		parent := interactiveRuns(t, w)[0].Env["AGGY_PARENT"]
		if parent == "" {
			t.Fatal("no AGGY_PARENT")
		}
		s = w.start(t, sc, "resume", parent)
		s.waitFor([]byte("READY>"))
		pick(s)
		s.waitWithin([]byte(note), offerTimeout)
		s.send([]byte{0x1c}) // the note's key opens the list
		s.waitFor([]byte("Switching to grok"))
		s.send([]byte{0x1b}) // Esc there cancels
		time.Sleep(100 * time.Millisecond)
		s.send([]byte{0x04})
		if code := s.exit(); code != 0 {
			t.Fatalf("aggy exit = %d", code)
		}
		runs := interactiveRuns(t, w)
		if len(runs) != 2 || runs[0].Vendor != "claude" || runs[1].Vendor != "claude" {
			t.Fatalf("interactive runs = %+v, want claude, then claude resumed, and no switch", runs)
		}
		if strings.Contains(s.screen(), "/exit\r") {
			t.Errorf("the exit command reached the CLI: %q", s.screen())
		}
	})
}

// askThenStop is a hook command for a fake: Stop from vendor goes to aggy
// hook, ending the turn the skill ran in, since aggy offers no switch before
// a turn end; any other event names the vendor aggy switch asks for.
func askThenStop(t *testing.T, vendor adapter.Vendor, flags string) []string {
	t.Helper()
	script := `case "$1" in Stop) [ "$AGGY_VENDOR" = ` + string(vendor) + ` ] || exit 0; exec ` + aggySh(t) + ` hook --vendor ` + string(vendor) +
		` Stop ;; *) ` + aggySh(t) + ` switch ` + flags + ` "$1" >/dev/null 2>&1 || true ;; esac`
	return []string{"/bin/sh", "-c", script, "sh"}
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
