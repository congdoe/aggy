package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// Regression tests for the PR #4 review (C13, S9). Each fails on 522a431.

// TestRepoCopyRefusesGitSpellings (C13).
func TestRepoCopyRefusesGitSpellings(t *testing.T) {
	for _, p := range []string{
		".GIT/config",
		".Git",
		"sub/.git/hooks/pre-commit",
		"a/b/.GiT",
		".g\u200dit/config", // zero-width joiner, ignored by HFS+
		".git\u200b",        // zero-width space
		"bad\nname",
		"tab\there",
	} {
		if _, err := ParseRepo("[worktree]\ncopy = [" + quote(p) + "]\n"); err == nil {
			t.Errorf("copy %q accepted", p)
		}
	}
	for _, p := range []string{".env", "config/.gitignore", ".github/workflows/x.yml", "a.git/b"} {
		if _, err := ParseRepo("[worktree]\ncopy = [" + quote(p) + "]\n"); err != nil {
			t.Errorf("copy %q refused: %v", p, err)
		}
	}
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// TestConfigReadsAreCapped (S9): user and repo config must be regular
// files of at most MaxFileSize, not symlinks.
func TestConfigReadsAreCapped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, RepoDir), 0o700); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(dir, FileName)
	big := strings.Repeat("# padding\n", MaxFileSize/10+1)
	for _, tt := range []struct {
		name string
		load func() error
		path string
	}{
		{"user", func() error { _, err := Load(user); return err }, user},
		{"repo", func() error { _, err := LoadRepo(repo); return err }, RepoPath(repo)},
	} {
		if err := os.WriteFile(tt.path, []byte(big), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := tt.load(); !errors.Is(err, safefs.ErrTooLarge) {
			t.Errorf("%s: oversized config = %v, want ErrTooLarge", tt.name, err)
		}
		real := filepath.Join(t.TempDir(), "real.toml")
		if err := os.WriteFile(real, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(tt.path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, tt.path); err != nil {
			t.Fatal(err)
		}
		if err := tt.load(); !errors.Is(err, safefs.ErrNotRegular) {
			t.Errorf("%s: symlinked config = %v, want ErrNotRegular", tt.name, err)
		}
	}
}

// TestLoadRepoRefusesLinkedAggyDir (T6725 L5): a cloned repo's .aggy that
// is a symlink cannot point the repo config at another file on the disk.
// The user config's directory is held to the same rule.
func TestLoadRepoRefusesLinkedAggyDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", t.TempDir())
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, FileName), []byte("allowed_vendors = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(repo, RepoDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRepo(repo); !errors.Is(err, safefs.ErrLinkedDir) {
		t.Errorf("LoadRepo with a linked .aggy = %v, want ErrLinkedDir", err)
	}
	userDir := filepath.Join(base, "state")
	if err := os.Symlink(elsewhere, userDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(userDir, FileName)); !errors.Is(err, safefs.ErrLinkedDir) {
		t.Errorf("Load with a linked state dir = %v, want ErrLinkedDir", err)
	}
}
