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
	p, err := WriteFile(dir, id, "first")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, ".aggy", "briefs", id+".md"); p != want {
		t.Errorf("path = %s, want %s", p, want)
	}
	if _, err := WriteFile(dir, id, "second"); err != nil {
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
		if _, err := WriteFile(dir, bad, "x"); err == nil {
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
	if _, err := WriteFile(linked, id, "x"); !errors.Is(err, aggydir.ErrLink) {
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
	if _, err := WriteFile(dir, id, "x"); !errors.Is(err, aggydir.ErrLink) {
		t.Errorf("symlinked brief: err = %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("symlink target written: %q", b)
	}
}
