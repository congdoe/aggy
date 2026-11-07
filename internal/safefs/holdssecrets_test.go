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
	worktree := filepath.Join(home, ".aggy", "worktrees", "0123456789abcdef")
	for _, d := range []string{home, project, worktree, filepath.Join(home, ".aggy", "tools"), filepath.Join(home, ".codex"), filepath.Join(home, ".ssh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "AGGY_HOME"} {
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
		{"aggy's state", filepath.Join(home, ".aggy"), true},
		{"aggy's tools", filepath.Join(home, ".aggy", "tools"), true},
		{"every worktree", filepath.Join(home, ".aggy", "worktrees"), true},
		{"a borrow's worktree", worktree, false},
		{"inside a borrow's worktree", filepath.Join(worktree, "src"), false},
		{"a link from a worktree to aggy's state", link("wt-escape", filepath.Join(home, ".aggy", "tools")), true},
		{"gh", filepath.Join(home, ".config", "gh"), true},
		{"gcloud", filepath.Join(home, ".config", "gcloud"), true},
		{"kube", filepath.Join(home, ".kube"), true},
		{"docker", filepath.Join(home, ".docker", "x"), true},
		{"other config", filepath.Join(home, ".config", "nvim"), false},
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

// TestHoldsSecretsMovedState (T6952): $AGGY_HOME moves aggy's state and its
// worktrees with it.
func TestHoldsSecretsMovedState(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("AGGY_HOME", state)
	for dir, want := range map[string]bool{state: true, filepath.Join(state, "tools"): true, filepath.Join(state, "worktrees", "x"): false} {
		if got, err := HoldsSecrets(dir); err != nil || got != want {
			t.Errorf("HoldsSecrets(%s) = %v, %v; want %v", dir, got, err, want)
		}
	}
}
