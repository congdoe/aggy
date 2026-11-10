//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// firstRunWorld is a fresh HOME with the offer on and no Codex rules file, as
// on a machine where aggy never ran.
func firstRunWorld(t *testing.T) (*world, string) {
	t.Helper()
	w := newWorld(t)
	w.offerSetup = true
	rules := filepath.Join(w.home, ".codex", "rules", "aggy.rules")
	if err := os.Remove(rules); err != nil {
		t.Fatal(err)
	}
	return w, rules
}

// TestFirstRunAccepted (T6966): bare aggy on a fresh HOME shows one screen
// naming the files it changes, takes one answer, writes them through the
// manifest, and launches; aggy unwire restores them; the offer is not made again.
func TestFirstRunAccepted(t *testing.T) {
	w, rules := firstRunWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}
	s := w.start(t, sc)
	s.waitWithin([]byte("Set up now? [y/N]"), offerTimeout)
	if !strings.Contains(s.screen(), "~/.codex/rules/aggy.rules") {
		t.Errorf("the screen does not name the rules file: %q", s.screen())
	}
	if _, err := os.Stat(rules); err == nil {
		t.Fatal("the rules file was written before the answer")
	}
	s.send([]byte("y\r"))
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("exit = %d; screen %q", code, s.screen())
	}
	screen := s.screen()
	for _, want := range []string{"aggy unwire --run", "Left to you:", "trust a folder"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q: %q", want, screen)
		}
	}
	if _, err := os.Stat(rules); err != nil {
		t.Fatalf("the rules file was not written: %v", err)
	}
	run := regexp.MustCompile(`aggy unwire --run (\S+)`).FindStringSubmatch(screen)
	if run == nil {
		t.Fatal("no run id")
	}
	// unwire is for a person at a terminal.
	u := w.start(t, sc, "unwire", "--run", run[1])
	if code := u.exit(); code != 0 {
		t.Fatalf("unwire: %d %s", code, u.screen())
	}
	if _, err := os.Stat(rules); !os.IsNotExist(err) {
		t.Errorf("unwire left the rules file: %v", err)
	}

	s = w.start(t, sc)
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte{0x04})
	_ = s.exit()
	if strings.Contains(s.screen(), "Set up now?") {
		t.Error("the offer was made again")
	}
}

// TestFirstRunDeclined (T6966): declining still launches, with one note, which
// a second launch does not repeat, nor the hooks line it stands for; nothing is
// written to the CLIs' config.
func TestFirstRunDeclined(t *testing.T) {
	w, rules := firstRunWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}
	s := w.start(t, sc)
	s.waitWithin([]byte("Set up now? [y/N]"), offerTimeout)
	s.send([]byte("n\r"))
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(s.screen(), "aggy setup --wire") {
		t.Errorf("no note: %q", s.screen())
	}
	if _, err := os.Stat(rules); err == nil {
		t.Error("a declined offer wrote the rules file")
	}
	if _, err := os.Stat(filepath.Join(w.home, ".claude", "settings.json")); err == nil {
		t.Error("a declined offer wrote Claude's settings")
	}
	s = w.start(t, sc)
	s.waitWithin([]byte("READY>"), offerTimeout)
	s.send([]byte{0x04})
	_ = s.exit()
	for _, not := range []string{"Set up now?", "setup skipped", "hooks for claude are not installed"} {
		if strings.Contains(s.screen(), not) {
			t.Errorf("the second launch said %q: %q", not, s.screen())
		}
	}
}

// TestFirstRunNoOffer (T6966): no offer without a terminal, for a borrow child,
// or with words after aggy; none of them writes the CLIs' config.
func TestFirstRunNoOffer(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		args []string
		pipe bool
	}{
		{"no terminal", nil, []string{"-p", "hi"}, true},
		{"print mode at a terminal", nil, []string{"-p", "hi"}, false},
		{"a borrow child", []string{"AGGY_DELEGATED=1"}, nil, false},
		{"a vendor named", nil, []string{"claude"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, rules := firstRunWorld(t)
			w.env = tt.env
			sc := map[string]any{"raw_echo": true, "stdout": "READY>", "interactive_without_tty": true}
			var screen string
			if tt.pipe {
				_, out, errb := w.pipe(t, sc, "", tt.args...)
				screen = out + errb
			} else {
				s := w.start(t, sc, tt.args...)
				deadline := time.Now().Add(offerTimeout)
			wait:
				for time.Now().Before(deadline) {
					select {
					case <-s.wait():
						break wait
					default:
					}
					if strings.Contains(s.screen(), "READY>") {
						_, _ = s.master.Write([]byte{0x04}) // a print run may have exited already
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				<-s.wait()
				screen = s.screen()
			}
			if strings.Contains(screen, "Set up now?") {
				t.Errorf("offered: %q", screen)
			}
			if _, err := os.Stat(rules); err == nil {
				t.Error("the rules file was written")
			}
		})
	}
}
