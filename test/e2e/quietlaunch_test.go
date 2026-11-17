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
				shown := strings.Join(strings.Fields(s.screen()), " ")
				// #148 review H3: said until a hook of agy's has come.
				if !strings.Contains(shown, "load only once you do") {
					t.Errorf("no folder trust note before agy's first hook: %q", shown)
				}
				time.Sleep(drawnWait)
				s.send([]byte("\r"))
				s.waitWithin([]byte("agy (fake)"), offerTimeout)
				s.send([]byte("work\r"))
				s.waitWithin([]byte("ok: work"), offerTimeout)
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
// the second CLI's own output. A line that means a protection is off or
// missing this run still prints on a switch (#148 review H3): agy before any
// hook of its has come from the folder. TestHooksOffSaid has Claude's.
func TestQuietSwitch(t *testing.T) {
	agyWorld := func(t *testing.T) *world {
		w := agyWorldE2E(t, true)
		cfg := "[switch]\norder = [\"claude\", \"antigravity\"]\n"
		if err := os.WriteFile(filepath.Join(w.home, ".aggy", "config.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return w
	}
	for _, c := range []struct {
		from, to string
		world    func(t *testing.T) *world
		ready    string // the second CLI's first output
		want     string // "" for no aggy line between the two
	}{
		{"claude", "grok", readyWorld, "READY>", ""},
		{"claude", "codex", readyWorld, "READY>", ""},
		{"claude", "antigravity", agyWorld, "Do you trust the contents of this project?", "load only once you do"},
	} {
		t.Run(c.from+" to "+c.to, func(t *testing.T) {
			w := c.world(t)
			if c.want == "" {
				for _, v := range []string{c.from, c.to} {
					// Each CLI's first launch here, so neither has a
					// first-time note left to give.
					s := w.start(t, map[string]any{"stdout": "READY>"}, "run", v)
					s.waitWithin([]byte("READY>"), offerTimeout)
					s.send([]byte("/exit\r"))
					s.exit()
				}
			}
			sc := map[string]any{
				"stdout":        "READY>",
				"hook_command":  []string{buildAggy(t), "hook", "--vendor", c.from},
				"hooks":         []map[string]any{{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}"}}},
				"session_files": startedFiles(),
			}
			s := w.start(t, sc, "run", c.from)
			s.waitWithin([]byte("READY>"), offerTimeout)
			s.send([]byte("work\r"))
			s.waitWithin([]byte("work"), offerTimeout)
			s.send([]byte{0x1c})
			s.waitWithin([]byte("aggy: switch vendor"), offerTimeout)
			pickVendor(t, s, c.to, false)
			s.waitWithin([]byte("-> "+c.to+". Ctrl+"), offerTimeout)
			mark := len(s.screen())
			s.confirmPastGuard(time.Now())
			waitScreen(t, s, mark, c.ready)
			between := s.screen()[mark:]
			lines := aggyLines(between, c.ready)
			switch {
			case c.want == "" && len(lines) > 0:
				t.Errorf("aggy printed between %s and %s:\n%s", c.from, c.to, strings.Join(lines, "\n"))
			case c.want != "" && !strings.Contains(strings.Join(strings.Fields(strings.Join(lines, " ")), " "), c.want):
				t.Errorf("no %q between %s and %s:\n%s", c.want, c.from, c.to, strings.Join(lines, "\n"))
			}
			_ = s.cmd.Process.Kill()
		})
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

// TestHooksOffSaid (#148 review H1): with Claude's hooks turned off in the
// repo's settings, aggy's decision hook cannot run, so its asks on aggy's own
// files and Claude's settings files are off: the launch says so, and aggy
// doctor too.
func TestHooksOffSaid(t *testing.T) {
	w := readyWorld(t)
	if err := os.MkdirAll(filepath.Join(w.cwd, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.cwd, ".claude", "settings.json"), []byte(`{"disableAllHooks": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := w.start(t, map[string]any{"stdout": "READY>"}, "run", "claude")
	s.waitWithin([]byte("READY>"), offerTimeout)
	shown := strings.Join(strings.Fields(strings.Join(aggyLines(s.screen(), "READY>"), " ")), " ")
	s.send([]byte("/exit\r"))
	s.exit()
	if !strings.Contains(shown, "aggy's asks on writes to its own files and Claude's settings files are not on this session") {
		t.Errorf("no line at launch: %q", shown)
	}
	if _, doc := w.aggyCode(t, "doctor"); !strings.Contains(doc, "claude-hooks-off") {
		t.Errorf("doctor does not say it:\n%s", doc)
	}
}
