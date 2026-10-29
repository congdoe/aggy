//go:build unix

package execrun

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// TestExecToolSwappedDir (F74-1): a launch folder swapped for a symlink
// or another folder between the caller's Lstat and the move is refused,
// and the process stays where it was; the folder checked is moved into.
func TestExecToolSwappedDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	// Not executable: an exec that is reached fails, so the test process
	// is never replaced.
	prog := filepath.Join(base, "prog")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { beforeChdir = func() {} })
	for i, c := range []struct {
		name string
		swap func(dir, other string) error
		ok   bool
	}{
		{"unchanged", func(string, string) error { return nil }, true},
		{"a symlink", func(dir, other string) error {
			if err := os.Rename(dir, dir+".old"); err != nil {
				return err
			}
			return os.Symlink(other, dir)
		}, false},
		{"another folder", func(dir, other string) error {
			if err := os.Rename(dir, dir+".old"); err != nil {
				return err
			}
			return os.Rename(other, dir)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sub := filepath.Join(base, string(rune('a'+i)))
			dir, other := filepath.Join(sub, "launch"), filepath.Join(sub, "other")
			for _, d := range []string{dir, other} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.Lstat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(base); err != nil {
				t.Fatal(err)
			}
			beforeChdir = func() {
				if err := c.swap(dir, other); err != nil {
					t.Error(err)
				}
			}
			err = ExecTool(prog, nil, dir, want, nil)
			wd, _ := os.Getwd()
			switch {
			case c.ok && (!errors.Is(err, syscall.EACCES) || wd != dir):
				t.Errorf("exec err %v, cwd %s, want EACCES in %s", err, wd, dir)
			case !c.ok && (!errors.Is(err, safefs.ErrChanged) && !errors.Is(err, syscall.ELOOP) && !errors.Is(err, syscall.ENOTDIR) || wd != base):
				t.Errorf("exec err %v, cwd %s, want a refusal in %s", err, wd, base)
			}
		})
	}
}
