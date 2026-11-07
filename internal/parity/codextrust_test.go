package parity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestAddCodexTrust(t *testing.T) {
	const dir = "/Users/me/repo"
	for _, tc := range []struct {
		name, src, want string
		err             bool
	}{
		{name: "empty", src: "", want: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "keeps what is there", src: "# mine\nmodel = \"x\"\n\n[tui]\ntheme = \"y\" # dark\n",
			want: "# mine\nmodel = \"x\"\n\n[tui]\ntheme = \"y\" # dark\n\n[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "no final newline", src: "model = \"x\"", want: "model = \"x\"\n\n[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "another project", src: "[projects.\"/other\"]\ntrust_level = \"untrusted\"\n",
			want: "[projects.\"/other\"]\ntrust_level = \"untrusted\"\n\n[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "already", src: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n", want: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "own table without the key", src: "[projects.\"/Users/me/repo\"]\nother = 1\n", want: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\nother = 1\n"},
		{name: "says otherwise", src: "[projects.\"/Users/me/repo\"]\ntrust_level = \"untrusted\"\n", err: true},
		{name: "inline table", src: "projects = { \"/other\" = { trust_level = \"trusted\" } }\n", err: true},
		{name: "inline entry", src: "[projects]\n\"/Users/me/repo\" = { other = 1 }\n", err: true},
		{name: "bad toml", src: "= nope", err: true},
		{name: "wrong type", src: "projects = 3\n", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AddCodexTrust([]byte(tc.src), dir)
			if (err != nil) != tc.err {
				t.Fatalf("err %v", err)
			}
			if !tc.err && string(got) != tc.want {
				t.Errorf("got %q", got)
			}
			if err == nil {
				again, err := AddCodexTrust(got, dir)
				if err != nil || string(again) != string(got) {
					t.Errorf("not idempotent: %q, %v", again, err)
				}
			}
		})
	}
}

func TestAddCodexTrustConflictAndQuoting(t *testing.T) {
	if _, err := AddCodexTrust([]byte("[projects.\"/a\"]\ntrust_level = \"untrusted\"\n"), "/a"); !errors.Is(err, ErrCodexTrustConflict) {
		t.Errorf("%v", err)
	}
	if _, err := AddCodexTrust(nil, "/a\nb"); err == nil {
		t.Error("a line break must not reach the file")
	}
	const odd = `/a"b\c] x`
	got, err := AddCodexTrust(nil, odd)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if err := toml.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Projects[odd].TrustLevel != "trusted" || strings.Count(string(got), "[projects") != 1 {
		t.Errorf("%q", got)
	}
}

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
