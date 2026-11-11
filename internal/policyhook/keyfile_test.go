package policyhook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
)

// TestKeyFileAsks (T6931, #138 review M3): while OpenCode runs, its key file
// sits in aggy's run folder; another session's read, search, or shell command
// that reaches it asks, by spelling, by a link, and by a glob.
func TestKeyFileAsks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	state := filepath.Join(base, ".aggy")
	keys := filepath.Join(state, "run", "p.1234abcd.memory", "opencode", "keys")
	key := filepath.Join(keys, "anthropic")
	for _, d := range []string{root, keys} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(key, filepath.Join(root, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := plan()
	p.Root, p.Protected = root, []string{state}
	cases := []struct {
		name string
		call parity.Call
		ask  bool
	}{
		{"read the key", parity.Call{Kind: parity.CallRead, Text: key}, true},
		{"read the key, other case", parity.Call{Kind: parity.CallRead, Text: filepath.Join(state, "RUN", "p.1234abcd.memory", "opencode", "Keys", "anthropic")}, true},
		{"read through a link in the repo", parity.Call{Kind: parity.CallRead, Text: "notes.txt"}, true},
		{"read a repo file", parity.Call{Kind: parity.CallRead, Text: "main.go"}, false},
		{"read the launch config", parity.Call{Kind: parity.CallRead, Text: filepath.Join(state, "run", "p.1234abcd.memory", "opencode", "opencode.json")}, false},
		{"search the run folder", parity.Call{Kind: parity.CallSearch, Text: filepath.Join(state, "run")}, true},
		{"search the state folder", parity.Call{Kind: parity.CallSearch, Text: state}, true},
		{"glob under the run folder", parity.Call{Kind: parity.CallSearch, Text: filepath.Join(state, "run") + "/**/keys/*"}, true},
		{"glob in the repo", parity.Call{Kind: parity.CallSearch, Text: "**/*.go"}, false},
		{"shell names the key folder", parity.Call{Kind: parity.CallShell, Text: "cat opencode/keys/anthropic"}, true},
		{"shell globs aggy's folder", parity.Call{Kind: parity.CallShell, Text: "cat ~/.aggy/r*/*/opencode/k*/*"}, true},
		{"shell globs $AGGY_HOME", parity.Call{Kind: parity.CallShell, Text: `head "$AGGY_HOME"/r?n/*/*/*/*`}, true},
		{"shell glob in the repo", parity.Call{Kind: parity.CallShell, Text: "ls src/*.go"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Judge(p, []parity.Call{c.call}, "", true)
			if (got.Verdict == parity.VerdictAsk && got.Reason == parity.ReasonProtected) != c.ask {
				t.Errorf("%+v: got %+v, want a protected ask %v", c.call, got, c.ask)
			}
		})
	}
	// Hermes' read_file, by the key's path and through the link in the repo.
	for _, path := range []string{key, filepath.Join(root, "notes.txt")} {
		if d := JudgeHermes(p, hermesPayload(t, "read_file", map[string]any{"path": path})); d.Verdict != parity.VerdictAsk {
			t.Errorf("Hermes read of %s: %+v", path, d)
		}
	}
}
