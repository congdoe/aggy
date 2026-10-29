package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDirSwappedFolder (T6820): a folder of the tree swapped for a
// symlink to an outside folder after the walk looked at it is refused,
// and the outside file is never read; a swapped file is refused too.
func TestDirSwappedFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src, outside := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(filepath.Join(src, "a"), 0o700))
	must(os.WriteFile(filepath.Join(src, "a", "f"), []byte("in"), 0o600))
	must(os.WriteFile(filepath.Join(src, "g"), []byte("in"), 0o600))
	must(os.WriteFile(filepath.Join(outside, "f"), []byte("secret"), 0o600))
	must(os.WriteFile(filepath.Join(outside, "g"), []byte("secret"), 0o600))

	fi, err := Lstat(src)
	must(err)
	d, err := OpenDir(src, fi)
	must(err)
	defer func() { _ = d.Close() }()
	afi, err := d.Lstat("a")
	must(err)
	gfi, err := d.Lstat("g")
	must(err)

	must(os.Rename(filepath.Join(src, "a"), filepath.Join(src, "a.old")))
	must(os.Symlink(outside, filepath.Join(src, "a")))
	// Outside the tree: os.Root refuses the escape.
	if sub, err := d.OpenDir("a", afi); err == nil {
		_ = sub.Close()
		t.Error("a folder swapped for a symlink out of the tree was opened")
	}
	// Inside the tree: the identity check refuses it.
	must(os.Remove(filepath.Join(src, "a")))
	must(os.Mkdir(filepath.Join(src, "b"), 0o700))
	must(os.Symlink("b", filepath.Join(src, "a")))
	if sub, err := d.OpenDir("a", afi); !errors.Is(err, ErrChanged) {
		if sub != nil {
			_ = sub.Close()
		}
		t.Errorf("folder swapped for a symlink in the tree: %v, want ErrChanged", err)
	}

	must(os.Remove(filepath.Join(src, "g")))
	must(os.Symlink(filepath.Join(outside, "g"), filepath.Join(src, "g")))
	if b, err := d.ReadFileLimit("g", gfi, 100); err == nil {
		t.Errorf("swapped file read: %q", b)
	}

	if _, err := d.Lstat("../x"); err == nil {
		t.Error("a path, not an entry name, was allowed")
	}
	if _, err := OpenDir(filepath.Join(src, "a"), afi); err == nil {
		t.Error("OpenDir of a symlink was allowed")
	}
}
