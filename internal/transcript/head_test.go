package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestReadHeadFirstPrompt (T6965): the head of a session file, with a line the
// window cut dropped, gives the first prompt the user typed, skipping what the
// CLI injected.
func TestReadHeadFirstPrompt(t *testing.T) {
	tests := []struct {
		name string
		v    adapter.Vendor
		data string
		n    int64
		want string
	}{
		{"claude", adapter.Claude, claudeLines, 1 << 20, "fix the bug"},
		{"codex", adapter.Codex, codexLines, 1 << 20, "fix the bug"},
		{"grok", adapter.Grok, grokLines, 1 << 20, "fix the bug"},
		{"a first line past the window", adapter.Claude, `{"type":"user","message":{"content":"` + strings.Repeat("x", 100) + `"}}` + "\n", 50, ""},
		{"nothing typed", adapter.Claude, `{"type":"summary","summary":"t"}` + "\n", 1 << 20, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			p := filepath.Join(root, "d", "s.jsonl")
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			data, err := ReadHead(root, p, tt.n)
			if err != nil {
				t.Fatal(err)
			}
			if got := FirstPrompt(tt.v, data); got != tt.want {
				t.Errorf("FirstPrompt = %q, want %q", got, tt.want)
			}
		})
	}
	// A symlink on the way is refused, as for ReadTail.
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHead(root, filepath.Join(root, "d", "s.jsonl"), 10); err == nil {
		t.Error("ReadHead followed a symlink")
	}
}
