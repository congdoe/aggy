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

// TestWriteFileNoFollowIn (B15-1): the write lands only in the directory
// whose identity the caller took; a directory swapped for a symlink to
// elsewhere after that fails with ErrChanged and writes nothing there.
func TestWriteFileNoFollowIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	dir := filepath.Join(root, "public")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoFollowIn(fi, filepath.Join(dir, "a.png"), []byte("ok"), 0o644, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoFollowIn(nil, filepath.Join(dir, "b.png"), []byte("x"), 0o644, nil); err == nil {
		t.Error("no dir identity accepted")
	}
	// The directory is swapped for a symlink out of root.
	outside := t.TempDir()
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoFollowIn(fi, filepath.Join(dir, "c.png"), []byte("x"), 0o644, nil); !errors.Is(err, ErrChanged) {
		t.Errorf("swapped dir = %v, want ErrChanged", err)
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Errorf("wrote outside: %v", es)
	}
}

// TestMkdirChain (B15-8): the dirs are made through descriptors from
// root; a symlink at any level, or one swapped in after the caller
// looked, is refused, and nothing is made outside root.
func TestMkdirChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, outside := t.TempDir(), t.TempDir()
	want, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := MkdirChain(root, want, []string{"a", "b"}, 0o700)
	if err != nil || !fi.IsDir() {
		t.Fatalf("MkdirChain = %v, %v", fi, err)
	}
	if got, _ := os.Stat(filepath.Join(root, "a", "b")); !os.SameFile(got, fi) {
		t.Error("identity is not the last dir's")
	}
	if err := os.Symlink(outside, filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	if _, err := MkdirChain(root, want, []string{"l", "x"}, 0o700); !errors.Is(err, ErrNotDir) {
		t.Errorf("through a link = %v, want ErrNotDir", err)
	}
	for _, bad := range [][]string{{".."}, {"a/b"}, {""}} {
		if _, err := MkdirChain(root, want, bad, 0o700); err == nil {
			t.Errorf("elements %q accepted", bad)
		}
	}
	// A level swapped for a link after the caller's check.
	testHookBeforeAct = func(op string) {
		if op != "MkdirChain" {
			return
		}
		_ = os.RemoveAll(filepath.Join(root, "a"))
		_ = os.Symlink(outside, filepath.Join(root, "a"))
	}
	t.Cleanup(func() { testHookBeforeAct = nil })
	if _, err := MkdirChain(root, want, []string{"a", "c"}, 0o700); !errors.Is(err, ErrNotDir) {
		t.Errorf("swapped level = %v, want ErrNotDir", err)
	}
	testHookBeforeAct = nil
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Errorf("made %d entries outside root", len(es))
	}
	// A root that is not the one the caller looked at.
	other, _ := os.Lstat(outside)
	if _, err := MkdirChain(root, other, []string{"z"}, 0o700); !errors.Is(err, ErrChanged) {
		t.Errorf("wrong root = %v, want ErrChanged", err)
	}
}

// TestExpectNamesOp (B15-9): a refusal names the call that made it, not
// Symlink.
func TestExpectNamesOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	name := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteFileNoFollow(name, []byte("y"), 0o600, nil)
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrExist) || !errors.As(err, &pe) || pe.Op != "WriteFile" {
		t.Errorf("err = %v, want a WriteFile ErrExist", err)
	}
}
