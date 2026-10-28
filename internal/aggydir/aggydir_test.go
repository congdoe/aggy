//go:build unix

package aggydir

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestNoLinks (F2): a symlink at .aggy or at any level below it is
// refused for reads and writes, even one that stays inside the repo.
func TestNoLinks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []struct {
		name  string
		plant func(t *testing.T, repo, outside string)
	}{
		{".aggy out", func(t *testing.T, repo, outside string) { link(t, outside, filepath.Join(repo, ".aggy")) }},
		{"a level out", func(t *testing.T, repo, outside string) {
			link(t, outside, filepath.Join(repo, ".aggy", "assets"))
		}},
		{"a level in the repo", func(t *testing.T, repo, _ string) {
			if err := os.MkdirAll(filepath.Join(repo, "other", "x"), 0o700); err != nil {
				t.Fatal(err)
			}
			link(t, filepath.Join(repo, "other"), filepath.Join(repo, ".aggy", "assets"))
		}},
		{"the file", func(t *testing.T, repo, outside string) {
			link(t, filepath.Join(outside, "f.json"), filepath.Join(repo, ".aggy", "assets", "x", "f.json"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, outside := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(outside, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "f.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			tt.plant(t, repo, outside)
			if _, err := ReadFile(repo, "assets/x/f.json", 1<<10); err == nil {
				t.Error("ReadFile read through a link")
			}
			if tt.name == "the file" {
				return
			}
			if _, _, err := MkdirAll(repo, 0o700, "assets", "x", "y"); !errors.Is(err, ErrLink) {
				t.Errorf("MkdirAll = %v, want ErrLink", err)
			}
			if _, err := os.Stat(filepath.Join(outside, "x", "y")); err == nil {
				t.Error("made a dir outside")
			}
		})
	}
}

func TestReadAndMake(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	if _, err := ReadFile(repo, "task.md", 1<<10); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing = %v", err)
	}
	p, fi, err := MkdirAll(repo, 0o700, "assets", "x")
	if err != nil || p != filepath.Join(repo, ".aggy", "assets", "x") || !fi.IsDir() {
		t.Fatalf("MkdirAll = %s, %v, %v", p, fi, err)
	}
	if err := os.WriteFile(filepath.Join(p, "f.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(repo, "assets/x/f.json", 1<<10); err != nil || string(b) != "{}" {
		t.Errorf("ReadFile = %q, %v", b, err)
	}
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}
