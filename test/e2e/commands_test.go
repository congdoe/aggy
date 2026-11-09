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
// never reaches the CLI (no turn, no Stop) and, Claude signalling its approval
// prompts, switches at once with no status line: a caught command started no
// turn, so nothing waits for a turn end.
func TestTypedSwitchCaught(t *testing.T) {
	w, sc, log := commandWorld(t)
	s := w.start(t, sc, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte("work\r"))
	waitStops(t, log, 2) // the launch's and the turn's
	s.typeSlowly("/switch grok")
	s.send([]byte("\r"))
	w.waitRuns(t, s, 2)
	if got := stops(t, log); got != 2 {
		t.Errorf("Stop hooks = %d, want 2: the command reached Claude as a turn", got)
	}
	if strings.Contains(s.screen(), "Esc stays") {
		t.Errorf("a status line asked for a typed switch: %q", s.screen())
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
			s.waitFor([]byte("READY>"))
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

// TestTypedInboxPage (T6909): /inbox shows aggy inbox's output on the
// alternate screen, no turn spent, and Esc gives the CLI its screen back.
func TestTypedInboxPage(t *testing.T) {
	w, sc, log := commandWorld(t)
	s := w.start(t, sc, "run", "claude")
	s.waitFor([]byte("READY>"))
	waitStops(t, log, 1)
	s.typeSlowly("/inbox")
	s.send([]byte("\r"))
	s.waitFor([]byte("No unread reports."))
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
	s.waitFor([]byte("READY>"))
	waitStops(t, log, 1)
	s.typeSlowly("/switch")
	s.send([]byte("\r"))
	s.waitFor([]byte("aggy: switch vendor"))
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
