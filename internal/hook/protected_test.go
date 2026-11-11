package hook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestSameAsProtected (#133 re-review): identity fails closed. A protected
// path or a part of the target that cannot be read counts as protected; a
// linked parent counts as where it leads; a file not yet written under aggy's
// folder spelled in other case is aggy's folder where the file system folds
// case.
func TestSameAsProtected(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, other := filepath.Join(home, ".aggy"), filepath.Join(home, "other")
	for _, d := range []string{state, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(state, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("permission denied")
	failOn := func(bad string, f func(string) (fs.FileInfo, error)) func(string) (fs.FileInfo, error) {
		return func(p string) (fs.FileInfo, error) {
			if p == bad {
				return nil, broken
			}
			return f(p)
		}
	}
	_, foldErr := os.Lstat(filepath.Join(home, ".AGGY"))
	for _, c := range []struct {
		name        string
		p           string
		stat, lstat func(string) (fs.FileInfo, error)
		want        bool
		needFold    bool
	}{
		{"elsewhere", filepath.Join(other, "a.txt"), os.Stat, os.Lstat, false, false},
		{"inside, as spelled", filepath.Join(state, "trust.json"), os.Stat, os.Lstat, true, false},
		{"a protected path that cannot be read", filepath.Join(other, "a.txt"), failOn(state, os.Stat), os.Lstat, true, false},
		{"a part that cannot be read", filepath.Join(other, "a.txt"), os.Stat, failOn(filepath.Join(other, "a.txt"), os.Lstat), true, false},
		{"a parent that cannot be read", filepath.Join(other, "a.txt"), failOn(other, os.Stat), os.Lstat, true, false},
		{"a linked parent", filepath.Join(home, "link", "trust.json"), os.Stat, os.Lstat, true, false},
		{"not yet written, other case", filepath.Join(home, ".AGGY", "new.json"), os.Stat, os.Lstat, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.needFold && foldErr != nil {
				t.Skipf("this file system tells the spellings apart (%v)", foldErr)
			}
			if got := SameAsProtected(c.p, []string{state, filepath.Join(home, "absent")}, c.stat, c.lstat); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
