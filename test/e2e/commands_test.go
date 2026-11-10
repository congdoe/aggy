//go:build unix

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// commandWorld is a world with aggy set up, and a Claude scenario whose hooks
// go through aggy hook and are also counted, one line per event, in the file
// it returns: a line that reached the CLI as a turn adds a Stop.
func commandWorld(t *testing.T) (*world, map[string]any, string) {
	t.Helper()
	w := newWorld(t)
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	log := filepath.Join(w.home, "hooks.log")
	script := `[ "$AGGY_VENDOR" = claude ] || exit 0; echo "$1" >> '` + log + `'; exec ` + aggySh(t) + ` hook --vendor claude "$1"`
	sc := map[string]any{
		"stdout":        "READY>",
		"hook_command":  []string{"/bin/sh", "-c", script, "sh"},
		"hooks":         []map[string]any{{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}},
		"session_files": startedFiles(),
	}
	return w, sc, log
}

// stops counts the Stop hooks Claude has run.
func stops(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(b), "Stop\n")
}

// typeSlowly sends each key in its own write, as a person types.
func (s *session) typeSlowly(keys string) {
	for _, k := range strings.Split(keys, "") {
		s.send([]byte(k))
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

// waitStops waits until the CLI has run n Stop hooks.
func waitStops(t *testing.T, log string, n int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for stops(t, log) < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := stops(t, log); got != n {
		t.Fatalf("Stop hooks = %d, want %d", got, n)
	}
}

// TestTypedSwitchCaught (T6908): /switch grok typed in full at Claude's prompt
// never reaches the CLI (no turn, no Stop), and a caught command started no
// turn, so nothing waits for a turn end: the status line asks at once (R121:
// Ctrl+U clearing Claude's composer is not recorded live, so it still asks).
func TestTypedSwitchCaught(t *testing.T) {
	w, sc, log := commandWorld(t)
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	waitStops(t, log, 2) // the launch's and the turn's
	s.typeSlowly("/switch grok")
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

// TestTypedCommandNotCaught (T6908): a pasted command, and one the tracker
// cannot see whole (a cursor key in it), go to the CLI as typed: a turn, and
// no switch.
func TestTypedCommandNotCaught(t *testing.T) {
	for _, c := range []struct {
		name string
		keys []string
	}{
		{"pasted", []string{"\x1b[200~/switch grok\x1b[201~"}},
		{"a cursor key in it", []string{"/switch grok", "\x1b[D", "\x1b[C"}},
		{"history recall", []string{"\x1b[A"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, sc, log := commandWorld(t)
			s := w.start(t, sc, "run", "claude")
			s.waitWithin([]byte("READY>"), offerTimeout)
			s.send([]byte("work\r"))
			waitStops(t, log, 2)
			for _, k := range c.keys {
				if strings.HasPrefix(k, "\x1b") {
					s.send([]byte(k))
					time.Sleep(50 * time.Millisecond)
					continue
				}
				s.typeSlowly(k)
			}
			s.send([]byte("\r"))
			waitStops(t, log, 3) // it reached Claude as a turn
			time.Sleep(1500 * time.Millisecond)
			if runs := interactiveRuns(t, w); len(runs) != 1 {
				t.Fatalf("interactive runs = %+v, want claude alone", runs)
			}
			s.send([]byte("/exit\r"))
			if code := s.exit(); code != 0 {
				t.Fatalf("aggy exit = %d", code)
			}
		})
	}
}

// TestHookBlockedSwitch (T6910): a /switch the CLI's UserPromptSubmit hook
// reports (a pick from its menu, which the keys do not spell) was blocked
// before the model, so it ends no turn: the switch must not wait for one. It
// asks on the status line, since a hook can be forged, and the switch key
// confirms it. The scenario's turn after the launch sends no Stop, as a
// blocked prompt does not.
func TestHookBlockedSwitch(t *testing.T) {
	w := newWorld(t)
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	mark := filepath.Join(w.home, "launched")
	script := `[ "$AGGY_VENDOR" = claude ] || exit 0; case "$1" in ` +
		`Stop) [ -f '` + mark + `' ] && exit 0; exec ` + aggySh(t) + ` hook --vendor claude Stop ;; ` +
		`UserPromptSubmit) [ -f '` + mark + `' ] || { touch '` + mark + `'; exit 0; }; exec ` + aggySh(t) + ` hook --vendor claude UserPromptSubmit ;; esac`
	sc := map[string]any{
		"stdout":       "READY>",
		"hook_command": []string{"/bin/sh", "-c", script, "sh"},
		"hooks": []map[string]any{
			{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}},
			{"event": "UserPromptSubmit", "payload": map[string]any{"session_id": "{session_id}", "prompt": "/switch grok"}},
		},
		"session_files": startedFiles(),
	}
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond) // past the launch's quiet gap
	s.send([]byte("/switch grok\r"))    // one read: the keys do not catch it
	s.waitWithin([]byte("switch via hook -> grok. Ctrl+"), offerTimeout)
	s.confirmPastGuard(time.Now())
	w.waitRuns(t, s, 2)
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 2 || runs[1].Vendor != "grok" {
		t.Fatalf("interactive runs = %+v, want claude then grok", runs)
	}
}

// TestHookMenuPickTab (T6975): a /switch picked from Claude's menu with Tab
// (/sw, Tab, then the vendor), which the keys do not spell whole, is taken
// from the UserPromptSubmit hook, as the typed prefix and the arguments match
// the reported command; it asks on the status line as every hook's switch.
// The same keys with another vendor reported are refused: the hook does not
// block the prompt, and nothing switches.
func TestHookMenuPickTab(t *testing.T) {
	for _, c := range []struct {
		name, prompt string
		taken        bool
	}{
		{"the picked command", "/switch grok", true},
		{"other arguments", "/switch codex", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			setup := exec.Command(buildAggy(t), "setup", "--yes")
			setup.Dir = w.cwd
			setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
			if out, err := setup.CombinedOutput(); err != nil {
				t.Fatalf("aggy setup: %v\n%s", err, out)
			}
			mark := filepath.Join(w.home, "launched")
			answers := filepath.Join(w.home, "answers.log")
			script := `[ "$AGGY_VENDOR" = claude ] || exit 0; case "$1" in ` +
				`Stop) [ -f '` + mark + `' ] && exit 0; exec ` + aggySh(t) + ` hook --vendor claude Stop ;; ` +
				`UserPromptSubmit) [ -f '` + mark + `' ] || { touch '` + mark + `'; exit 0; }; ` +
				aggySh(t) + ` hook --vendor claude UserPromptSubmit > '` + answers + `.tmp'; mv '` + answers + `.tmp' '` + answers + `' ;; esac`
			sc := map[string]any{
				"stdout":       "READY>",
				"hook_command": []string{"/bin/sh", "-c", script, "sh"},
				"hooks": []map[string]any{
					{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}},
					{"event": "UserPromptSubmit", "payload": map[string]any{"session_id": "{session_id}", "prompt": c.prompt}},
				},
				"session_files": startedFiles(),
			}
			s := w.start(t, sc, "run", "claude")
			s.waitWithin([]byte("READY>"), offerTimeout)
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(mark); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(1500 * time.Millisecond) // past the launch's quiet gap
			s.typeSlowly("/sw")
			s.send([]byte("\t"))
			time.Sleep(50 * time.Millisecond)
			s.typeSlowly("grok")
			s.send([]byte("\r"))
			if c.taken {
				s.waitWithin([]byte("switch via hook -> grok. Ctrl+"), offerTimeout)
				s.confirmPastGuard(time.Now())
				w.waitRuns(t, s, 2)
			} else {
				deadline := time.Now().Add(timeout)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(answers); err == nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				b, err := os.ReadFile(answers)
				if err != nil {
					t.Fatalf("the hook never answered: %v", err)
				}
				if strings.Contains(string(b), "block") {
					t.Fatalf("the hook blocked a prompt aggy refused: %q", b)
				}
				time.Sleep(500 * time.Millisecond)
				if runs := interactiveRuns(t, w); len(runs) != 1 {
					t.Fatalf("interactive runs = %+v, want claude alone", runs)
				}
			}
			s.send([]byte("/exit\r"))
			if code := s.exit(); code != 0 {
				t.Fatalf("aggy exit = %d", code)
			}
			if runs := interactiveRuns(t, w); c.taken && (len(runs) != 2 || runs[1].Vendor != "grok") {
				t.Fatalf("interactive runs = %+v, want claude then grok", runs)
			}
		})
	}
}

// TestTypedInboxPage (T6909): /inbox shows aggy inbox's output on the
// alternate screen, no turn spent, and Esc gives the CLI its screen back.
func TestTypedInboxPage(t *testing.T) {
	w, sc, log := commandWorld(t)
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	waitStops(t, log, 1)
	s.typeSlowly("/inbox")
	s.send([]byte("\r"))
	s.waitWithin([]byte("No unread reports."), offerTimeout)
	s.send([]byte("\x1b"))
	time.Sleep(300 * time.Millisecond)
	if got := stops(t, log); got != 1 {
		t.Errorf("Stop hooks = %d, want 1: /inbox reached Claude", got)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	s.restored("after the page")
}

// TestTypedPickerOpens (T6908): /switch alone opens the picker, as the switch
// key does, and Esc there leaves nothing waiting: the CLI exits on its own.
func TestTypedPickerOpens(t *testing.T) {
	w, sc, log := commandWorld(t)
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	waitStops(t, log, 1)
	s.typeSlowly("/switch")
	s.send([]byte("\r"))
	s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
	s.send([]byte("\x1b"))
	time.Sleep(300 * time.Millisecond)
	if got := stops(t, log); got != 1 {
		t.Errorf("Stop hooks = %d, want 1: /switch reached Claude", got)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 1 {
		t.Fatalf("interactive runs = %+v, want claude alone", runs)
	}
}
