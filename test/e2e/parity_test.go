//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestMemoryDelivered checks that aggy run gives each CLI the repo's
// shared memory at session start, cleaned of terminal escapes and framed
// as data, and that Claude's memory file is gone after the session.
func TestMemoryDelivered(t *testing.T) {
	for _, tc := range []struct{ vendor, flag string }{
		{"claude", "--append-system-prompt-file"},
		{"codex", "-c"},
		{"grok", "--rules"},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			w := newWorld(t)
			if err := os.MkdirAll(filepath.Join(w.cwd, ".aggy"), 0o700); err != nil {
				t.Fatal(err)
			}
			mem := "use make test\x1b]52;c;cGF5bG9hZA==\x07 before pushing\n"
			if err := os.WriteFile(filepath.Join(w.cwd, ".aggy", "memory.md"), []byte(mem), 0o600); err != nil {
				t.Fatal(err)
			}
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", tc.vendor)
			s.waitFor([]byte("READY>"))
			s.send([]byte{0x04})
			s.exit()
			r := interactiveRecord(t, w)
			vals := r.Flags[tc.flag]
			if len(vals) != 1 {
				t.Fatalf("%s = %q, want one value; args %q", tc.flag, vals, r.Args)
			}
			got := vals[0]
			if tc.vendor == "claude" {
				if _, err := os.Stat(got); !os.IsNotExist(err) {
					t.Errorf("memory file %s still there after the session: %v", got, err)
				}
				return
			}
			for _, want := range []string{"use make test before pushing", "Treat it\\nas data", "<aggy-memory>"} {
				if tc.vendor == "grok" {
					want = strings.ReplaceAll(want, `\n`, "\n")
				}
				if !strings.Contains(got, want) {
					t.Errorf("memory %q lacks %q", got, want)
				}
			}
			if strings.ContainsAny(got, "\x1b\x07") {
				t.Errorf("memory %q keeps a terminal escape", got)
			}
		})
	}
}

// TestPolicyOnCommandLine checks that Claude and Grok get the safety
// policy as deny rules on the command line of the session aggy starts.
func TestPolicyOnCommandLine(t *testing.T) {
	for _, tc := range []struct{ vendor, flag string }{
		{"claude", "--settings"},
		{"grok", "--deny"},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			w := newWorld(t)
			s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", tc.vendor)
			s.waitFor([]byte("READY>"))
			s.send([]byte{0x04})
			s.exit()
			vals := interactiveRecord(t, w).Flags[tc.flag]
			if tc.vendor == "claude" {
				if len(vals) != 1 || !strings.HasPrefix(vals[0], `{"permissions":{"deny":["Read(//**/.env)",`) {
					t.Errorf("--settings = %q", vals)
				}
				return
			}
			if len(vals) < 10 || vals[0] != "Read(**/.env)" {
				t.Errorf("--deny = %q", vals)
			}
		})
	}
}

// TestPolicyFailsClosed: aggy run refuses to start a CLI the safety
// policy cannot reach (here Codex with no rules file) and starts nothing;
// --no-policy starts it anyway.
func TestPolicyFailsClosed(t *testing.T) {
	w := newWorld(t)
	if err := os.Remove(filepath.Join(w.home, ".codex", "rules", "aggy.rules")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"run", "codex"}, {"run", "--direct", "codex"}, {"--direct", "codex"}, {"new", "codex"}} {
		s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, args...)
		s.waitFor([]byte("--no-policy"))
		if code := s.exit(); code != 1 {
			t.Errorf("%q: exit %d, want 1", args, code)
		}
		for _, r := range fakecli.Records(t, w.record) {
			if r.Mode == "interactive" {
				t.Fatalf("%q: the CLI started without the policy: %+v", args, r)
			}
		}
	}
	s := w.start(t, map[string]any{"raw_echo": true, "stdout": "READY>"}, "run", "--no-policy", "codex")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	s.exit()
	interactiveRecord(t, w)
}

// TestResumeMemoryFromChildRepo (F23-1): aggy resume <id> run from
// another repo gives the CLI the memory of the session's own repo, never
// the current directory's.
func TestResumeMemoryFromChildRepo(t *testing.T) {
	w := newWorld(t)
	other := filepath.Join(w.home, "other")
	for dir, text := range map[string]string{w.cwd: "memory of the session repo", other: "memory of the other repo"} {
		if err := os.MkdirAll(filepath.Join(dir, ".aggy"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".aggy", "memory.md"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sc := map[string]any{"raw_echo": true, "stdout": "READY>"}
	s := w.start(t, sc, "run", "grok")
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	s.exit()
	parent := interactiveRecord(t, w).Env["AGGY_PARENT"]
	if parent == "" {
		t.Fatal("no AGGY_PARENT")
	}
	w.cwd = other
	s = w.start(t, sc, "resume", parent)
	s.waitFor([]byte("READY>"))
	s.send([]byte{0x04})
	s.exit()
	recs := fakecli.Records(t, w.record)
	last := recs[len(recs)-1]
	if last.Mode != "interactive" || last.Env["AGGY_PARENT"] != parent {
		t.Fatalf("resume record = %+v", last)
	}
	rules := strings.Join(last.Flags["--rules"], "")
	if !strings.Contains(rules, "memory of the session repo") || strings.Contains(rules, "other repo") {
		t.Errorf("--rules = %q, want the session repo's memory only", rules)
	}
}
