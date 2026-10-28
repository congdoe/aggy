package home

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDir(t *testing.T) {
	h := t.TempDir()
	override := t.TempDir()
	tests := []struct {
		name, aggyHome, want string
	}{
		{"default", "", filepath.Join(h, ".aggy")},
		{"override", override, override},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", h)
			t.Setenv(Env, tt.aggyHome)
			got, err := Dir()
			if err != nil || got != tt.want {
				t.Errorf("Dir() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

// TestDirRefusesRelative (PR #6): a relative AGGY_HOME fails closed.
func TestDirRefusesRelative(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(Env, "state")
	if d, err := Dir(); err == nil {
		t.Errorf("Dir = %q, want an error for a relative %s", d, Env)
	}
}

// TestDirResolvesLinkedRoot (PR #6): a symlinked state directory is
// resolved once, to the real directory, for dotfile setups.
func TestDirResolvesLinkedRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	real := filepath.Join(base, "dotfiles", "aggy")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(Env, link)
	want, _ := filepath.EvalSymlinks(real)
	if d, err := Dir(); err != nil || d != want {
		t.Errorf("Dir = %q, %v, want %q", d, err, want)
	}
	plain := filepath.Join(base, "plain")
	t.Setenv(Env, plain)
	if d, err := Dir(); err != nil || d != plain {
		t.Errorf("Dir(missing, not a link) = %q, %v, want %q unchanged", d, err, plain)
	}
}
