//go:build linux || darwin

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestRenameNoReplaceFallback (T6820): on a file system that refuses the
// no-replace flag (EINVAL, as NFS and 9p do), a regular file still moves,
// with link and unlink, and a name made after the check is kept; a
// folder falls back to the checked rename.
func TestRenameNoReplaceFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	atomicRenameFn = func(int, string, string) error { return syscall.EINVAL }
	t.Cleanup(func() { atomicRenameFn = atomicRename })
	dir := t.TempDir()
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "<" + err.Error() + ">"
		}
		return string(b)
	}

	// A file moves; the old name is gone.
	write("a.tmp", "new")
	fi, _ := os.Lstat(filepath.Join(dir, "a.tmp"))
	if err := RenameNoReplace(filepath.Join(dir, "a.tmp"), filepath.Join(dir, "a"), fi); err != nil {
		t.Fatal(err)
	}
	if read("a") != "new" {
		t.Errorf("a = %q", read("a"))
	}
	if _, err := os.Lstat(filepath.Join(dir, "a.tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the old name is still there: %v", err)
	}

	// A file made at the target after the check is kept.
	write("b.tmp", "new")
	fi, _ = os.Lstat(filepath.Join(dir, "b.tmp"))
	testHookBeforeRename = func(string) { write("b", "old") }
	err := RenameNoReplace(filepath.Join(dir, "b.tmp"), filepath.Join(dir, "b"), fi)
	testHookBeforeRename = nil
	if !errors.Is(err, fs.ErrExist) || read("b") != "old" || read("b.tmp") != "new" {
		t.Errorf("racer: err %v, b %q, b.tmp %q", err, read("b"), read("b.tmp"))
	}

	// A folder: the checked rename.
	if err := os.Mkdir(filepath.Join(dir, "d.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Lstat(filepath.Join(dir, "d.tmp"))
	if err := RenameNoReplace(filepath.Join(dir, "d.tmp"), filepath.Join(dir, "d"), fi); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Lstat(filepath.Join(dir, "d")); err != nil || !os.SameFile(fi, got) {
		t.Errorf("folder not moved: %v", err)
	}
}
