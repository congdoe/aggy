//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
