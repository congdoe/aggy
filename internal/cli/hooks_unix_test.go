//go:build unix

package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedPath(t *testing.T) {
	if err := trustedPath("/bin/sh"); err != nil {
		t.Errorf("/bin/sh: %v", err)
	}
	d := t.TempDir()
	open := filepath.Join(d, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(open, "aggy")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(bin); err == nil || !strings.Contains(err.Error(), "writable by others") {
		t.Errorf("world-writable dir: %v", err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(bin); err == nil {
		t.Error("group-writable binary passed")
	}
	// A link from a safe place into a writable one is judged by its target.
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "aggy")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(link); err == nil {
		t.Error("a link into a writable dir passed")
	}
	_ = os.Chmod(open, 0o755)

	// A sticky folder such as /tmp is fine above a folder of this user's
	// (CI builds aggy under /tmp), but not as the binary's only guard
	// against a group- or world-writable folder of its own.
	sticky := filepath.Join(d, "sticky")
	own := filepath.Join(sticky, "own")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(own, "aggy")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(bin); err != nil {
		t.Errorf("below a sticky folder: %v", err)
	}
	if err := os.Chmod(own, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(bin); err == nil {
		t.Error("a world-writable folder below a sticky one passed")
	}
	_ = os.Chmod(own, 0o700)
}

// TestTrustedEntry: every entry on the way, a symlink included, must be
// the user's or root's; a symlink of another user's in a sticky folder
// could be repointed by that user.
func TestTrustedEntry(t *testing.T) {
	const me, other = 501, 502
	for _, c := range []struct {
		name string
		mode fs.FileMode
		uid  int
		leaf bool
		ok   bool
	}{
		{"own symlink", fs.ModeSymlink | 0o777, me, false, true},
		{"root's symlink", fs.ModeSymlink | 0o777, 0, false, true},
		{"another user's symlink", fs.ModeSymlink | 0o777, other, false, false},
		{"unknown owner", fs.ModeDir | 0o755, -1, false, false},
		{"sticky root folder", fs.ModeDir | fs.ModeSticky | 0o777, 0, false, true},
		{"another user's sticky folder", fs.ModeDir | fs.ModeSticky | 0o777, other, false, false},
		{"open folder", fs.ModeDir | 0o777, me, false, false},
		{"group-writable binary", 0o775, me, true, false},
		{"own binary", 0o755, me, true, true},
	} {
		if err := trustedEntry("/x", c.mode, c.uid, me, c.leaf); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
