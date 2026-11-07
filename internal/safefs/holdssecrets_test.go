package safefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHoldsSecrets (T6952): a home, what holds it, and the folders of keys in
// it count, however they are spelled; a project does not.
func TestHoldsSecrets(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "users", "me")
	project := filepath.Join(root, "srv", "project")
	for _, d := range []string{home, project, filepath.Join(home, ".codex"), filepath.Join(home, ".ssh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(v, "")
	}
	link := func(name, target string) string {
		p := filepath.Join(root, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"home", home, true},
		{"above home", filepath.Dir(home), true},
		{"the root", "/", true},
		{"a vendor folder", filepath.Join(home, ".codex"), true},
		{"inside a vendor folder", filepath.Join(home, ".codex", "x"), true},
		{"a credential file", filepath.Join(home, ".codex", "auth.json"), true},
		{"ssh keys", filepath.Join(home, ".ssh"), true},
		{"a link to home", link("to-home", home), true},
		{"a link to the root", link("to-root", "/"), true},
		{"a link to ssh keys", link("to-ssh", filepath.Join(home, ".ssh")), true},
		{"a project", project, false},
		{"a folder under home", filepath.Join(home, "src", "x"), false},
		{"a link to a project", link("to-project", project), false},
		{"a folder not made yet", filepath.Join(root, "srv", "new"), false},
	}
	if foldCase {
		tests = append(tests, struct {
			name string
			dir  string
			want bool
		}{"home in another case", strings.ToUpper(home), true})
	}
	if stripFirmlink {
		tests = append(tests, struct {
			name string
			dir  string
			want bool
		}{"home through the firmlink", dataVolume + home, true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HoldsSecrets(tt.dir)
			if err != nil || got != tt.want {
				t.Errorf("HoldsSecrets(%s) = %v, %v; want %v", tt.dir, got, err, tt.want)
			}
		})
	}
}
