package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRefusesAntigravityTrees (T6957): every path under ~/.gemini and
// ~/.antigravity is refused, the folders themselves too, existing or not, by
// any spelling, and through a symlink; a sibling folder is not.
func TestRefusesAntigravityTrees(t *testing.T) {
	tests := []struct {
		name string
		path func(home string) string
	}{
		{"gemini folder", func(h string) string { return filepath.Join(h, ".gemini") }},
		{"gemini settings", func(h string) string { return filepath.Join(h, ".gemini", "antigravity-cli", "settings.json") }},
		{"gemini transcript", func(h string) string {
			return filepath.Join(h, ".gemini", "antigravity-cli", "brain", "00000000-0000-0000-0000-000000000000", "transcript.jsonl")
		}},
		{"gemini global hooks", func(h string) string { return filepath.Join(h, ".gemini", "config", "hooks.json") }},
		{"gemini token guess", func(h string) string { return filepath.Join(h, ".gemini", "oauth_creds.json") }},
		{"antigravity folder", func(h string) string { return filepath.Join(h, ".antigravity", "x") }},
		{"unclean", func(h string) string { return h + "/.gemini//./config/../x" }},
	}
	for _, tt := range tests {
		for _, exists := range []bool{false, true} {
			name := tt.name
			if exists {
				name += " existing"
			}
			t.Run(name, func(t *testing.T) {
				home := fakeHome(t)
				p := tt.path(home)
				if exists {
					clean := filepath.Clean(p)
					if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
						t.Fatal(err)
					}
					if fi, err := os.Stat(clean); err != nil || !fi.IsDir() {
						if err := os.WriteFile(clean, []byte("x"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err := ReadFile(p); !errors.Is(err, ErrAuthPath) {
					t.Errorf("ReadFile(%s) = %v, want ErrAuthPath", p, err)
				}
				if _, err := Stat(p); !errors.Is(err, ErrAuthPath) {
					t.Errorf("Stat(%s) = %v, want ErrAuthPath", p, err)
				}
				if err := WriteFile(p, []byte("y"), 0o600); !errors.Is(err, ErrAuthPath) {
					t.Errorf("WriteFile(%s) = %v, want ErrAuthPath", p, err)
				}
			})
		}
	}
}

// TestAntigravityTreeByLinkAndIdentity: a symlink into the tree and the
// folder's own identity under another name are refused; a sibling passes, and
// HoldsSecrets takes the tree for a folder of keys without taking the home.
func TestAntigravityTreeByLinkAndIdentity(t *testing.T) {
	home := fakeHome(t)
	tree := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(tree, "settings.json")
	if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	link := filepath.Join(other, "s.json")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(link); !errors.Is(err, ErrAuthPath) {
		t.Errorf("ReadFile through a link = %v, want ErrAuthPath", err)
	}
	dirLink := filepath.Join(other, "g")
	if err := os.Symlink(filepath.Join(home, ".gemini"), dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := Stat(dirLink + "/antigravity-cli/settings.json"); !errors.Is(err, ErrAuthPath) {
		t.Errorf("Stat through a folder link = %v, want ErrAuthPath", err)
	}
	sib := filepath.Join(home, ".geminix")
	if err := WriteFile(sib, []byte("ok"), 0o600); err != nil {
		t.Errorf("a sibling is refused: %v", err)
	}
	held, err := HoldsSecrets(filepath.Join(home, "project"))
	if err != nil || held {
		t.Errorf("HoldsSecrets(a home's project) = %v, %v; want false", held, err)
	}
	held, err = HoldsSecrets(filepath.Join(home, ".gemini"))
	if err != nil || !held {
		t.Errorf("HoldsSecrets(~/.gemini) = %v, %v; want true", held, err)
	}
}
