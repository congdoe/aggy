//go:build unix

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/fakecli"
)

// checkedClaude is the Claude Code version the live checks of its composer were
// recorded on (docs/verification.md, "Claude Code's Ctrl+U and /exit"): only
// there does a typed switch go in one step (adapter.ClearLineChecked).
const checkedClaude = "2.1.322"

// commandWorld is a world with aggy set up, and a Claude scenario at
// checkedClaude whose hooks go through aggy hook and are also counted, one line
// per event, in the file it returns: a line that reached the CLI as a turn adds
// a Stop.
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
	claudeVersion(t, w, checkedClaude)
	return w, sc, log
}

// claudeVersion makes the fake claude in w report version to --version, as
// aggy's launch probe runs it with no scenario; "" keeps the fake's default.
func claudeVersion(t *testing.T, w *world, version string) {
	t.Helper()
	if version == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(w.bin, "claude.version"), []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lineModes counts the fake CLI's lines by how they ended: "turn" reached the
// model, "prompt-blocked" a UserPromptSubmit hook blocked.
func lineModes(t *testing.T, w *world) map[string]int {
	t.Helper()
	m := map[string]int{}
	for _, r := range fakecli.Records(t, w.record) {
		if r.Mode == "turn" || r.Mode == "prompt-blocked" {
			m[r.Mode]++
		}
	}
	return m
}

// keyedOffer fails the test unless the switch was offered keyed, in one step
// with no status line (switch.request.offered, attrs.keyed), and never asked.
func keyedOffer(t *testing.T, w *world) {
	t.Helper()
	code, out := w.aggyCode(t, "diag", "export", "--jsonl", "--event", "switch.request.offered")
	if code != 0 {
		t.Fatalf("aggy diag export: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, `"keyed":true`) || strings.Contains(out, `"asked":`) {
		t.Errorf("switch offers = %q, want it keyed, with no status line", out)
	}
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
// turn, so nothing waits for a turn end: the switch goes at once, in one step,
// with no status line (Ctrl+U's byte clearing Claude's composer is recorded
// live, docs/verification.md, so the typed command is the confirmation).
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
	w.waitRuns(t, s, 2)
	keyedOffer(t, w)
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

// TestDownThenSwitchCaught (#146 review, the owner's first line): on Claude at
// the checked version, Down on the empty prompt is recorded live to change
// nothing (adapter.EmptyComposerKey), so Down, then /switch grok typed in full
// and Enter, is caught from the keys at once and goes in one step: no turn, no
// question, no status line.
func TestDownThenSwitchCaught(t *testing.T) {
	w, sc, log := commandWorld(t)
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte("work\r"))
	waitStops(t, log, 2)
	waitTurnEnds(t, w, 2)
	s.send([]byte("\x1b[B"))
	time.Sleep(50 * time.Millisecond)
	s.typeSlowly("/switch grok")
	s.waitWithin([]byte("/switch grok"), offerTimeout)
	s.send([]byte("\r"))
	w.waitRuns(t, s, 2)
	keyedOffer(t, w)
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
}

// TestStrayKeyTyped (T6982, the coordinator's live run on Claude 2.1.322): a
// stray key with no live check (Up) at the prompt, then /switch and Enter. The
// key catch loses the line, and the prompt hook reports /switch: it must never
// reach the model. The person typed that very line (typedMatch, #146 review,
// third round), so aggy blocks the prompt and acts at once: the switch list
// opens. A message after it runs a turn, and /switch codex typed in full is
// then caught from the keys and switches in one step.
func TestStrayKeyTyped(t *testing.T) {
	w := newWorld(t)
	setup := exec.Command(buildAggy(t), "setup", "--yes")
	setup.Dir = w.cwd
	setup.Env = []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("aggy setup: %v\n%s", err, out)
	}
	answers := filepath.Join(w.home, "answers.log")
	// A turn takes longer than the supervisor's settleTurnGap, as a real one
	// does, so its Stop settles the composer.
	script := `[ "$AGGY_VENDOR" = claude ] || exit 0; case "$1" in ` +
		`Stop) sleep 1.2; exec ` + aggySh(t) + ` hook --vendor claude Stop ;; ` +
		`UserPromptSubmit) ` + aggySh(t) + ` hook --vendor claude UserPromptSubmit | tee -a '` + answers + `' ;; esac`
	sc := map[string]any{
		"stdout":       "READY>",
		"hook_command": []string{"/bin/sh", "-c", script, "sh"},
		"hooks": []map[string]any{
			{"event": "UserPromptSubmit", "payload": map[string]any{"session_id": "{session_id}", "prompt": "{prompt}"}},
			{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}},
		},
		"session_files": startedFiles(),
	}
	claudeVersion(t, w, checkedClaude)
	s := w.start(t, sc, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	waitTurnEnds(t, w, 1)
	time.Sleep(1500 * time.Millisecond) // past the launch's quiet gap

	s.send([]byte("\x1b[A"))
	time.Sleep(50 * time.Millisecond)
	s.typeSlowly("/switch")
	s.send([]byte("\r"))
	s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
	b, err := os.ReadFile(answers)
	if err != nil || !strings.Contains(string(b), `"block"`) {
		t.Fatalf("the hook's answer %q (%v): want a block", b, err)
	}
	if m := lineModes(t, w); m["prompt-blocked"] != 1 || m["turn"] != 0 {
		t.Fatalf("lines %v: want the /switch blocked before the model", m)
	}
	s.send([]byte("\x1b")) // the list's Esc
	time.Sleep(300 * time.Millisecond)

	s.typeSlowly("fix the bug")
	s.send([]byte("\r"))
	waitTurnEnds(t, w, 2)
	s.typeSlowly("/switch codex")
	s.waitWithin([]byte("/switch codex"), offerTimeout)
	s.send([]byte("\r"))
	w.waitRuns(t, s, 2)
	keyedOffer(t, w)
	if m := lineModes(t, w); m["prompt-blocked"] != 1 || m["turn"] != 1 {
		t.Errorf("lines %v: want one blocked and one turn", m)
	}
	code, out := w.aggyCode(t, "diag", "export", "--jsonl", "--event", "supervisor.command.refused")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Errorf("a hook's command was refused and passed: exit %d %q", code, out)
	}
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if runs := interactiveRuns(t, w); len(runs) != 2 || runs[1].Vendor != "codex" {
		t.Fatalf("interactive runs = %+v, want claude then codex", runs)
	}
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
// sends none. Claude reports version (the fake's default when ""). It waits
// until the launch's prompt hook has run, and past the launch's quiet gap.
func promptHookWorld(t *testing.T, prompt, version string) (*world, *session, string) {
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
		`Stop) [ -f '` + mark + `.stop' ] && exit 0; touch '` + mark + `.stop'; exec ` + aggySh(t) + ` hook --vendor claude Stop ;; ` +
		`UserPromptSubmit) [ -f '` + mark + `' ] || { touch '` + mark + `'; exit 0; }; ` +
		aggySh(t) + ` hook --vendor claude UserPromptSubmit | tee '` + answers + `.tmp'; mv '` + answers + `.tmp' '` + answers + `' ;; esac`
	sc := map[string]any{
		"stdout":       "READY>",
		"hook_command": []string{"/bin/sh", "-c", script, "sh"},
		// The fake stops a line whose UserPromptSubmit printed a block before
		// the hooks after it (a Stop), as Claude does (T6982).
		"hooks": []map[string]any{
			{"event": "UserPromptSubmit", "payload": map[string]any{"session_id": "{session_id}", "prompt": prompt}},
			{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}},
		},
		"session_files": startedFiles(),
	}
	claudeVersion(t, w, version)
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
// The same keys with another vendor reported are not the person's, and spell
// another line, from which a turn may be running: aggy runs nothing and asks
// nothing, but the prompt is blocked all the same, never reaching the model,
// and the bottom row says why and the keyboard's way (T6982).
func TestHookMenuPickTab(t *testing.T) {
	for _, c := range []struct {
		name, prompt string
		taken        bool
	}{
		{"the picked command", "/switch grok", true},
		{"other arguments", "/switch codex", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, s, answers := promptHookWorld(t, c.prompt, "")
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
				if b := hookAnswer(t, answers); !strings.Contains(b, `"block"`) || !strings.Contains(b, "did not run /switch") {
					t.Fatalf("the hook let a command aggy did not run through to the model: %q", b)
				}
				s.waitWithin([]byte("aggy: did not run /switch"), offerTimeout)
				if strings.Contains(s.screen(), "from claude's prompt?") {
					t.Fatalf("aggy asked about a refused command: %q", s.screen())
				}
				if m := lineModes(t, w); m["prompt-blocked"] != 1 || m["turn"] != 0 {
					t.Fatalf("lines %v: want the one prompt blocked before the model", m)
				}
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

// TestHookTypedActs (T6982, the owner's second line in Apple Terminal): a
// Backspace on the empty prompt, then /switch grok typed and Enter. The
// Backspace spoils the line for the key catch at a version with no live check
// of it, and before T6982 the prompt hook's /switch grok went to the model. Now
// the hook blocks the prompt, and since the keys typed exactly that line
// (typedMatch, #146 review, third round) aggy switches as on one typed in
// full; with no live fact for a blocked prompt at this version, the switch
// asks on the status line before /exit is typed.
func TestHookTypedActs(t *testing.T) {
	w, s, answers := promptHookWorld(t, "/switch grok", "")
	s.send([]byte("\x7f"))
	time.Sleep(50 * time.Millisecond)
	s.typeSlowly("/switch grok")
	s.send([]byte("\r"))
	s.waitWithin([]byte("-> grok. Ctrl+"), offerTimeout)
	if b := hookAnswer(t, answers); !strings.Contains(b, `"block"`) || strings.Contains(b, "asked about") {
		t.Fatalf("the hook let the prompt through, or asked: %q", b)
	}
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

// TestHookUnvouchedAsks (T6982): a Backspace on the empty prompt, then
// /switch grok with its Enter in the same read (no typedMatch: a person's
// Enter comes alone). The hook blocks the prompt (its reason says aggy
// asked), aggy asks on its status line, and the switch key, the only yes,
// runs the switch as one typed in full; aggy typed no Ctrl+U for it, so the
// switch asks on the status line before /exit is typed.
func TestHookUnvouchedAsks(t *testing.T) {
	w, s, answers := promptHookWorld(t, "/switch grok", "")
	s.send([]byte("\x7f"))
	time.Sleep(50 * time.Millisecond)
	s.send([]byte("/switch grok\r"))
	s.waitWithin([]byte("/switch grok from claude's prompt? Ctrl+"), offerTimeout)
	if b := hookAnswer(t, answers); !strings.Contains(b, `"block"`) || !strings.Contains(b, "asked about") {
		t.Fatalf("the hook let the prompt through, or gave the wrong reason: %q", b)
	}
	s.confirmPastGuard(time.Now())
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
