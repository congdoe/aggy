//go:build unix

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ansiRE matches the terminal's control sequences, so a screen reads as text.
var ansiRE = regexp.MustCompile(`\x1b(\[[0-9;?<>=]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[()][0-9A-Za-z]|[=>78DEHMNOZc])`)

// aggyLines are the lines of screen before marker that aggy printed: those
// that start with "aggy". The CLI's own output starts at marker.
func aggyLines(screen, marker string) []string {
	if i := strings.Index(screen, marker); i >= 0 {
		screen = screen[:i]
	}
	var out []string
	for _, l := range strings.Split(ansiRE.ReplaceAllString(screen, ""), "\n") {
		l = strings.TrimSpace(strings.ReplaceAll(l, "\r", ""))
		if strings.HasPrefix(l, "aggy") {
			out = append(out, l)
		}
	}
	return out
}

// readyWorld is a world in a git repo with aggy set up for the three main
// CLIs as a person who said yes to everything would have it: aggy setup, the
// hooks installed, and aggy's command skills linked.
func readyWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	gi := exec.Command("/usr/bin/git", "init", "-q", w.cwd)
	gi.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin"}
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	w.aggyIn(t, "setup", "--yes")
	w.aggyIn(t, "hooks", "install")
	w.aggyIn(t, "skill", "link")
	return w
}

// TestQuietLaunch (T7015): an ordinary launch of each vendor, with everything
// set up, prints no line of aggy's before the CLI starts. The first launch in
// a folder may say what it set up or ask (agy's folder trust, Hermes' hook
// consent); the next says nothing. What it used to say is in aggy doctor.
func TestQuietLaunch(t *testing.T) {
	for _, c := range []struct {
		vendor string
		world  func(t *testing.T) *world
		sc     map[string]any
		ready  string
		// first answers the first launch's questions and waits for ready.
		first func(t *testing.T, s *session)
	}{
		{"claude", readyWorld, map[string]any{"stdout": "READY>"}, "READY>", nil},
		{"codex", readyWorld, map[string]any{"stdout": "READY>"}, "READY>", nil},
		{"grok", readyWorld, map[string]any{"stdout": "READY>"}, "READY>", nil},
		{"opencode", func(t *testing.T) *world { return openCodeWorldE2E(t) }, map[string]any{"stdout": "READY>"}, "READY>", nil},
		{"hermes", func(t *testing.T) *world {
			w := hermesWorldE2E(t, true)
			// The person's own Hermes config no longer adopts other CLIs'
			// logins, as aggy's warning about it says to do.
			dir := filepath.Join(w.home, ".hermes")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("auth:\n  adopt_external_logins: false\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return w
		}, map[string]any{"stdout": "READY>", "hermes": map[string]any{"approve": "y"}},
			"Hermes Agent (fake)", func(t *testing.T, s *session) {
				s.waitWithin([]byte("Event:   on_session_end"), offerTimeout)
				s.send([]byte("y\r"))
				s.waitWithin([]byte("Event:   pre_tool_call"), offerTimeout)
				s.send([]byte("y\r"))
			}},
		{"antigravity", func(t *testing.T) *world { return agyWorldE2E(t, true) }, map[string]any{"stdout": "READY>", "agy": map[string]any{"approve": "y"}},
			"agy (fake)", func(t *testing.T, s *session) {
				s.waitWithin([]byte("Do you trust the contents of this project?"), offerTimeout)
				s.send([]byte("\r"))
			}},
	} {
		t.Run(c.vendor, func(t *testing.T) {
			w := c.world(t)
			for i := range 2 {
				s := w.start(t, c.sc, "run", c.vendor)
				if i == 0 && c.first != nil {
					c.first(t, s)
				}
				s.waitWithin([]byte(c.ready), offerTimeout)
				time.Sleep(100 * time.Millisecond)
				shown := s.screen()
				s.send([]byte("/exit\r"))
				if code := s.exit(); code != 0 {
					t.Fatalf("launch %d: exit %d\n%s", i, code, shown)
				}
				if i == 0 {
					continue
				}
				if lines := aggyLines(shown, c.ready); len(lines) > 0 {
					t.Errorf("aggy printed before %s started:\n%s", c.vendor, strings.Join(lines, "\n"))
				}
			}
		})
	}
}

// TestQuietSwitch (T7015): a switch from one CLI to the next, everything set
// up, prints nothing of aggy's between them: no status line or note before
// the second CLI's own output.
func TestQuietSwitch(t *testing.T) {
	w := readyWorld(t)
	for _, v := range []string{"claude", "grok"} {
		// Each CLI's first launch here, so neither switch target has a
		// first-time note left to give.
		s := w.start(t, map[string]any{"stdout": "READY>"}, "run", v)
		s.waitWithin([]byte("READY>"), offerTimeout)
		s.send([]byte("/exit\r"))
		s.exit()
	}
	sc := map[string]any{
		"stdout":        "READY>",
		"hook_command":  []string{buildAggy(t), "hook", "--vendor", "claude"},
		"hooks":         []map[string]any{{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}},
		"session_files": startedFiles(),
	}
	s := w.start(t, sc, "run", "claude")
	s.waitFor([]byte("READY>"))
	s.send([]byte("work\r"))
	s.waitFor([]byte("work"))
	s.send([]byte{0x1c})
	s.waitFor([]byte("aggy: switch vendor"))
	pickVendor(t, s, "grok", false)
	s.waitWithin([]byte("-> grok. Ctrl+"), offerTimeout)
	mark := len(s.screen())
	s.confirmPastGuard(time.Now())
	w.waitRuns(t, s, 2)
	waitScreen(t, s, mark, "READY>")
	between := s.screen()[mark:]
	s.send([]byte("/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}
	if lines := aggyLines(between, "READY>"); len(lines) > 0 {
		t.Errorf("aggy printed between claude and grok:\n%s", strings.Join(lines, "\n"))
	}
	if _, err := os.Stat(filepath.Join(w.cwd, ".aggy", "briefs")); err == nil {
		t.Error("a brief was written for a switch without one")
	}
}

// waitScreen waits until the screen after mark shows want.
func waitScreen(t *testing.T, s *session, mark int, want string) {
	t.Helper()
	deadline := time.Now().Add(offerTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(s.screen()[mark:], want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q after the switch:\n%q", want, s.screen()[mark:])
}
