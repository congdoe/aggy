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

// waitTurnEnds waits until the supervisor's loop has counted n turn ends of
// the CLI on screen, by its diagnostic records: the hooks log sees a Stop
// before the supervisor does.
func waitTurnEnds(t *testing.T, w *world, n int) {
	t.Helper()
	got := 0
	for end := time.Now().Add(offerTimeout); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		code, out := w.aggyCode(t, "diag", "export", "--jsonl", "--event", "hook.event.accepted")
		if code != 0 {
			t.Fatalf("aggy diag export: exit %d\n%s", code, out)
		}
		got = strings.Count(out, `"counted":true`)
		if got >= n {
			return
		}
	}
	t.Fatalf("turn ends counted = %d, want %d", got, n)
}

// TestTypedSwitchCaught (T6908): /switch grok typed in full at Claude's prompt
// never reaches the CLI (no turn, no Stop), and a caught command started no
// turn, so nothing waits for a turn end: the status line asks at once (R121:
// Ctrl+U clearing Claude's composer is not recorded live, so it still asks).
//
// It waits on events, not durations (it failed once under load): the hooks
// log counts a Stop before aggy hook sends it, so the test also waits until
// the supervisor's loop has counted both turn ends; and the Enter is sent
// only once the CLI has echoed the whole line, so the supervisor has read
// every key and the Enter comes alone in its read, as catchCommand requires.
func TestTypedSwitchCaught(t *testing.T) {
	w, sc, log := commandWorld(t)
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	waitStops(t, log, 2) // the launch's and the turn's
	waitTurnEnds(t, w, 2)
	s.typeSlowly("/switch grok")
	s.waitWithin([]byte("/switch grok"), offerTimeout)
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

// promptHookWorld is a world with aggy set up and a Claude scenario whose prompt
// hook reports prompt for every line after the launch, its answer kept in the
// file it returns, and whose Stop counts only at the launch, as a blocked prompt
// sends none. It waits until the launch's prompt hook has run, and past the
// launch's quiet gap.
func promptHookWorld(t *testing.T, prompt string) (*world, *session, string) {
	t.Helper()
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
			{"event": "UserPromptSubmit", "payload": map[string]any{"session_id": "{session_id}", "prompt": prompt}},
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
	return w, s, answers
}

// hookAnswer waits for the prompt hook's answer and returns it.
func hookAnswer(t *testing.T, answers string) string {
	t.Helper()
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
	return string(b)
}

// TestHookMenuPickTab (T6975): a /switch picked from Claude's menu with Tab
// (/sw, Tab, then the vendor), which the keys do not spell whole, is taken
// from the UserPromptSubmit hook, as the typed prefix and the arguments match
// the reported command; it asks on the status line as every hook's switch.
// The same keys with another vendor reported are not the person's: since
// T6982 the hook blocks that prompt too, so it reaches no model, and aggy asks
// whether to run it; Esc drops it, and nothing switches.
func TestHookMenuPickTab(t *testing.T) {
	for _, c := range []struct {
		name, prompt string
		taken        bool
	}{
		{"the picked command", "/switch grok", true},
		{"other arguments", "/switch codex", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, s, answers := promptHookWorld(t, c.prompt)
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
				s.waitWithin([]byte("/switch codex from claude's prompt? Enter or Ctrl+"), offerTimeout)
				if b := hookAnswer(t, answers); !strings.Contains(b, `"block"`) {
					t.Fatalf("the hook let a prompt aggy asks about through: %q", b)
				}
				s.send([]byte("\x1b"))
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

// TestHookUnvouchedAsks (T6982, the owner's run in Apple Terminal): a Down on
// the empty prompt, then /switch grok and Enter. The keys vouch for no
// command, so the key catch passes the line, and before T6982 the prompt
// hook's /switch grok was refused and went to the model. Now the hook blocks
// the prompt, aggy asks on its status line, and one Enter there runs the
// switch as one typed in full.
func TestHookUnvouchedAsks(t *testing.T) {
	w, s, answers := promptHookWorld(t, "/switch grok")
	s.send([]byte("\x1b[B"))
	time.Sleep(50 * time.Millisecond)
	s.typeSlowly("/switch grok")
	s.send([]byte("\r"))
	s.waitWithin([]byte("/switch grok from claude's prompt? Enter or Ctrl+"), offerTimeout)
	if b := hookAnswer(t, answers); !strings.Contains(b, `"block"`) {
		t.Fatalf("the hook let the prompt through to the model: %q", b)
	}
	seen := time.Now()
	for time.Since(seen) < 600*time.Millisecond {
		time.Sleep(10 * time.Millisecond)
	}
	s.send([]byte("\r"))
	// Ctrl+U clearing Claude's composer is not recorded live, so the switch
	// still asks on the status line before /exit is typed.
	s.waitWithin([]byte("-> grok. Ctrl+"), offerTimeout)
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
