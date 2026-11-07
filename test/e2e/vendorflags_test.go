//go:build unix

package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestLooseningFlagNoted (T6903): a person's own bypass flag needs --no-policy,
// then reaches the CLI with a launch note and a record that names it.
func TestLooseningFlagNoted(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>", "extra_switches": []string{"--dangerously-skip-permissions"}}
	s := w.start(t, sc, "claude", "--dangerously-skip-permissions")
	s.waitFor([]byte("cli.flag_needs_no_policy"))
	if code := s.exit(); code != 2 {
		t.Errorf("without --no-policy: exit = %d, want 2", code)
	}
	if n := len(fakecli.Records(t, w.record)); n != 0 {
		t.Errorf("the CLI started %d times without --no-policy", n)
	}

	s = w.start(t, sc, "claude", "--no-policy", "--dangerously-skip-permissions")
	s.waitFor([]byte("loosen approvals"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("with --no-policy: exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	if !slices.Contains(recs[len(recs)-1].Args, "--dangerously-skip-permissions") {
		t.Errorf("args = %q", recs[len(recs)-1].Args)
	}
	code, out := w.aggyCode(t, "diag", "--json")
	if code != 0 || !strings.Contains(out, "loosening_flags") || !strings.Contains(out, "--dangerously-skip-permissions") {
		t.Errorf("diag = %d, %s", code, out)
	}
}

// TestResumeFlagAgrees (T6903): aggy -r with no id refuses when nothing here
// can be reopened, as aggy claude -r does, and starts nothing.
func TestResumeFlagAgrees(t *testing.T) {
	for _, args := range [][]string{{"-r"}, {"claude", "-r"}, {"--continue"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			w := newWorld(t)
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, args...)
			s.waitFor([]byte("cli.not_found"))
			if code := s.exit(); code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if n := len(fakecli.Records(t, w.record)); n != 0 {
				t.Errorf("the CLI started %d times", n)
			}
		})
	}
}

// TestSafeModeNoted (T6952): a flag worth a line at launch reaches the CLI
// without --no-policy, and the person sees the line.
func TestSafeModeNoted(t *testing.T) {
	w := newWorld(t)
	sc := map[string]any{"raw_echo": true, "stdout": "READY>", "extra_switches": []string{"--safe-mode"}}
	s := w.start(t, sc, "claude", "--safe-mode")
	s.waitFor([]byte("may not run"))
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	if code := s.exit(); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	recs := fakecli.Records(t, w.record)
	if !slices.Contains(recs[len(recs)-1].Args, "--safe-mode") {
		t.Errorf("args = %q", recs[len(recs)-1].Args)
	}
}
