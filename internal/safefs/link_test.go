package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileNoFollowKeepsRacer (B14-2): with want nil, a file made at
// the name after the check and before the write is kept, and the call
// fails with ErrExist; nothing is left under a temp name.
func TestWriteFileNoFollowKeepsRacer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	name := filepath.Join(dir, "restored")
	testHookBeforeCommit = func(string) {
		if err := os.WriteFile(name, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testHookBeforeCommit = nil })
	err := WriteFileNoFollow(name, []byte("ours"), 0o600, nil)
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("WriteFileNoFollow = %v, want ErrExist", err)
	}
	if b, _ := os.ReadFile(name); string(b) != "theirs" {
		t.Errorf("the racer's file = %q, want kept", b)
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("left %d entries, want only the racer's file", len(es))
	}
	// Without a racer the file is written with one link and perm.
	testHookBeforeCommit = nil
	fresh := filepath.Join(dir, "fresh")
	if err := WriteFileNoFollow(fresh, []byte("ours"), 0o640, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(fresh); err != nil || string(b) != "ours" {
		t.Errorf("fresh = %q, %v", b, err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o640 {
		t.Errorf("perm = %v", fi.Mode().Perm())
	}
}

// TestReadFileNoFollow (B14-1): the file itself, never through a link,
// and only with the identity the caller saw.
func TestReadFileNoFollow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	other := filepath.Join(dir, "o")
	link := filepath.Join(dir, "l")
	for p, b := range map[string]string{file: "f", other: "o"} {
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	fiFile, _ := os.Lstat(file)
	fiOther, _ := os.Lstat(other)
	if b, err := ReadFileNoFollow(file, fiFile); err != nil || string(b) != "f" {
		t.Errorf("plain read = %q, %v", b, err)
	}
	if _, err := ReadFileNoFollow(file, fiOther); !errors.Is(err, ErrChanged) {
		t.Errorf("another identity = %v, want ErrChanged", err)
	}
	if b, err := ReadFileNoFollow(link, fiOther); err == nil {
		t.Errorf("read through a link = %q", b)
	}
}
