package parity

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCodexTrustLevel (T6899): the level recorded for the folder or its repo
// root, "" for none, and an error for a file that is not TOML.
func TestCodexTrustLevel(t *testing.T) {
	tests := []struct {
		name, config, cwd, root, want string
		wantErr                       bool
	}{
		{name: "no config", cwd: "/r/a", root: "/r"},
		{name: "no projects table", config: "model = \"x\"\n", cwd: "/r", root: "/r"},
		{name: "the folder", config: "[projects.\"/r/a\"]\ntrust_level = \"trusted\"\n", cwd: "/r/a", root: "/r", want: "trusted"},
		{name: "the repo root", config: "[projects.\"/r\"]\ntrust_level = \"trusted\"\n", cwd: "/r/a", root: "/r", want: "trusted"},
		{name: "untrusted", config: "[projects.\"/r\"]\ntrust_level = \"untrusted\"\n", cwd: "/r", want: "untrusted"},
		{name: "another folder", config: "[projects.\"/x\"]\ntrust_level = \"trusted\"\n", cwd: "/r", root: "/r"},
		{name: "not a string", config: "[projects.\"/r\"]\ntrust_level = 1\n", cwd: "/r"},
		{name: "not TOML", config: "[[[", cwd: "/r", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
			if err := os.MkdirAll(filepath.Join(home, "codex"), 0o700); err != nil {
				t.Fatal(err)
			}
			if tt.config != "" {
				if err := os.WriteFile(filepath.Join(home, "codex", "config.toml"), []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := CodexTrustLevel(tt.cwd, tt.root)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("got %q, %v; want %q, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
