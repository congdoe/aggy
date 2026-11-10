//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantLogged (T6925) runs aggy log and fails unless it prints a row of
// event typ whose detail holds detail: a switch the fakes completed, or the
// CLI that came on screen.
func wantLogged(t *testing.T, w *world, typ, detail string) {
	t.Helper()
	code, out := w.aggyCode(t, "log")
	if code != 0 {
		t.Fatalf("aggy log: exit %d\n%s", code, out)
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) > 4 && f[3] == typ && strings.Contains(l, detail) {
			return
		}
	}
	t.Errorf("aggy log has no %s row with %q:\n%s", typ, detail, out)
}

// loggedRows counts aggy log's rows of event typ.
func loggedRows(t *testing.T, w *world, typ string) int {
	t.Helper()
	code, out := w.aggyCode(t, "log")
	if code != 0 {
		t.Fatalf("aggy log: exit %d\n%s", code, out)
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 4 && f[3] == typ {
			n++
		}
	}
	return n
}

// TestLaunchEndsBeforeCLINoRow (T6925): a launch that ends after it is
// recorded but before its CLI runs writes no screen row: a brief that
// cannot be written, and a CLI that cannot be started. A launch refused
// before it is recorded writes none either.
func TestLaunchEndsBeforeCLINoRow(t *testing.T) {
	t.Run("brief not written", func(t *testing.T) {
		w := newWorld(t)
		s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "claude")
		s.waitFor([]byte("READY>"))
		s.send([]byte{0x04})
		if code := s.exit(); code != 0 {
			t.Fatalf("aggy exit = %d", code)
		}
		if n := loggedRows(t, w, "screen"); n != 1 {
			t.Fatalf("screen rows after the first launch = %d, want 1", n)
		}
		briefs := filepath.Join(w.cwd, ".aggy", "briefs")
		if err := os.MkdirAll(briefs, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(w.home, "elsewhere"), filepath.Join(briefs, ".gitignore")); err != nil {
			t.Fatal(err)
		}
		s = w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "resume", "--brief")
		if code := s.exit(); code == 0 {
			t.Fatalf("aggy resume --brief exit 0 through a planted link: %q", s.screen())
		}
		if n := loggedRows(t, w, "screen"); n != 1 {
			t.Errorf("screen rows = %d, want only the first launch's; terminal: %q", n, s.screen())
		}
	})
	broken := func(t *testing.T, w *world) {
		// Executable, so it is found on PATH, but it cannot be started.
		if err := os.WriteFile(filepath.Join(w.bin, "claude"), []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("CLI not started", func(t *testing.T) {
		w := newWorld(t)
		broken(t, w)
		s := w.start(t, map[string]any{}, "run", "--no-policy", "claude")
		if code := s.exit(); code == 0 {
			t.Fatalf("aggy exit 0 with a CLI that cannot start: %q", s.screen())
		}
		if !strings.Contains(s.screen(), "supervisor.launch_failed") {
			t.Errorf("the start did not fail as launch_failed: %q", s.screen())
		}
		if n := loggedRows(t, w, "screen"); n != 0 {
			t.Errorf("screen rows = %d, want 0; terminal: %q", n, s.screen())
		}
	})
	t.Run("refused", func(t *testing.T) {
		w := newWorld(t)
		broken(t, w) // its help cannot be read, so the policy cannot reach it
		s := w.start(t, map[string]any{}, "run", "claude")
		if code := s.exit(); code == 0 {
			t.Fatalf("aggy exit 0: %q", s.screen())
		}
		if n := loggedRows(t, w, "screen"); n != 0 {
			t.Errorf("screen rows = %d, want 0; terminal: %q", n, s.screen())
		}
	})
}
