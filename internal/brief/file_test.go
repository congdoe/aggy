package brief

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/aggydir"
)

// T6674: a brief lands in .aggy/briefs/<child-id>.md, replaces the last
// one, and never goes through a symlink.
func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	const id = "0123abcd-0000-4000-8000-00000000beef"
	p, _, err := WriteFile(dir, id, "first")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".aggy", "briefs", id+".md"); p != want {
		t.Errorf("path = %s, want %s", p, want)
	}
	// G34-4: the briefs folder keeps itself out of git, and the repo's
	// own files are left alone.
	if b, err := os.ReadFile(filepath.Join(dir, ".aggy", "briefs", ".gitignore")); err != nil || string(b) != "*\n" {
		t.Errorf(".gitignore = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("wrote the repo's .gitignore: %v", err)
	}
	if pp, err := Path(dir, id); err != nil || pp != p {
		t.Errorf("Path = %s, %v", pp, err)
	}
	if _, _, err := WriteFile(dir, id, "second"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "second" {
		t.Errorf("read %q, %v", b, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
	for _, bad := range []string{"", "../x", "a/b", "ABC"} {
		if _, _, err := WriteFile(dir, bad, "x"); err == nil {
			t.Errorf("child id %q accepted", bad)
		}
	}

	// A symlinked briefs folder or brief file is refused, and its target
	// left alone.
	outside := t.TempDir()
	linked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(linked, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linked, ".aggy", "briefs")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteFile(linked, id, "x"); !errors.Is(err, aggydir.ErrLink) {
		t.Errorf("symlinked briefs dir: err = %v", err)
	}
	target := filepath.Join(outside, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteFile(dir, id, "x"); !errors.Is(err, aggydir.ErrLink) {
		t.Errorf("symlinked brief: err = %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("symlink target written: %q", b)
	}

	// A symlinked .gitignore in the briefs folder is refused.
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".aggy", "briefs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(other, ".aggy", "briefs", ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteFile(other, id, "x"); !errors.Is(err, aggydir.ErrLink) {
		t.Errorf("symlinked .gitignore: err = %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("symlink target written: %q", b)
	}
}

// G34-6: a .gitignore already in the briefs folder that is not "*" is
// kept, with a warning; "*" or the one aggy wrote gives none.
func TestWriteFileIgnoreWarn(t *testing.T) {
	const id = "0123abcd-0000-4000-8000-00000000beef"
	for _, tc := range []struct {
		name, content string
		warn          bool
	}{
		{"star", "*\n", false},
		{"star no newline", "*", false},
		{"empty", "", true},
		{"unignore", "!*\n", true},
		{"star then unignore", "*\n!*.md\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ignore := filepath.Join(dir, ".aggy", "briefs", ".gitignore")
			if err := os.MkdirAll(filepath.Dir(ignore), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ignore, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, warn, err := WriteFile(dir, id, "x")
			if err != nil || (warn != "") != tc.warn {
				t.Errorf("warn = %q, err = %v; want a warning: %v", warn, err, tc.warn)
			}
			if b, _ := os.ReadFile(ignore); string(b) != tc.content {
				t.Errorf(".gitignore changed to %q", b)
			}
		})
	}
	if _, warn, err := WriteFile(t.TempDir(), id, "x"); err != nil || warn != "" {
		t.Errorf("fresh folder: warn = %q, err = %v", warn, err)
	}
}
